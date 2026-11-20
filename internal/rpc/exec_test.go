// In-process stream scaffolding intentionally favors direct test construction.
//
//nolint:containedctx,testpackage,wsl_v5
package rpc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/cirruslabs/tart-guest-agent/pkg/v1"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	execTestShell   = "/bin/sh"
	execTestTimeout = 5 * time.Second
	execTestPIDEnv  = "PID_FILE"
)

type execTestStream struct {
	grpc.ServerStream

	ctx       context.Context
	requests  chan *v1.ExecRequest
	responses chan *v1.ExecResponse
	sendHook  func(*v1.ExecResponse) error
}

var _ grpc.BidiStreamingServer[v1.ExecRequest, v1.ExecResponse] = (*execTestStream)(nil)

func newExecTestStream(ctx context.Context) *execTestStream {
	return &execTestStream{
		ctx:       ctx,
		requests:  make(chan *v1.ExecRequest, 8),
		responses: make(chan *v1.ExecResponse, 8),
	}
}

func (stream *execTestStream) Send(response *v1.ExecResponse) error {
	if stream.sendHook != nil {
		if err := stream.sendHook(response); err != nil {
			return err
		}
	}

	select {
	case stream.responses <- response:
		return nil
	case <-stream.ctx.Done():
		return stream.ctx.Err()
	}
}

func (stream *execTestStream) Recv() (*v1.ExecRequest, error) {
	select {
	case request, ok := <-stream.requests:
		if !ok {
			return nil, io.EOF
		}
		return request, nil
	case <-stream.ctx.Done():
		return nil, stream.ctx.Err()
	}
}

func (stream *execTestStream) Context() context.Context { return stream.ctx }

func TestExecSendsStartedBeforeOutputAndExit(t *testing.T) {
	_, stream, result := startExecTest(t, &v1.ExecRequest_Command{
		Name: execTestShell,
		Args: []string{"-c", "printf hello"},
	})

	first := receiveExecResponse(t, stream)
	require.NotNil(t, first.GetStarted())

	var output []byte
	for {
		response := receiveExecResponse(t, stream)
		switch response := response.GetType().(type) {
		case *v1.ExecResponse_StandardOutput:
			output = append(output, response.StandardOutput.GetData()...)
		case *v1.ExecResponse_Exit_:
			require.EqualValues(t, 0, response.Exit.GetCode())
			require.Equal(t, []byte("hello"), output)
			require.NoError(t, receiveExecResult(t, result))
			return
		default:
			t.Fatalf("unexpected exec response %T", response)
		}
	}
}

func TestWaitWhileExecOutputIsPending(t *testing.T) {
	for _, test := range []struct {
		command string
		code    int32
	}{
		{
			command: "exit 0",
			code:    0,
		},
		{
			command: "exit 17",
			code:    17,
		},
		{
			command: "kill -TERM $$",
			code:    signalExitCodeOffset + int32(syscall.SIGTERM),
		},
	} {
		t.Run(test.command, func(t *testing.T) {
			client := v1.NewAgentClient(newExecGRPCTestConn(t))
			ctx, cancel := context.WithTimeout(t.Context(), execTestTimeout)
			defer cancel()

			stream, err := client.Exec(ctx)
			require.NoError(t, err)

			// The child inherits stderr and writes more than the gRPC receive window.
			const outputBytes = 8 << 20
			err = stream.Send(&v1.ExecRequest{
				Type: &v1.ExecRequest_Command_{
					Command: &v1.ExecRequest_Command{
						Name: execTestShell,
						Args: []string{
							"-c",
							`read start; head -c "$1" /dev/zero >&2 & ` + test.command,
							"sh",
							strconv.Itoa(outputBytes),
						},
						Interactive: true,
					},
				},
			})
			require.NoError(t, err)

			first, err := stream.Recv()
			require.NoError(t, err)
			request := &v1.WaitRequest{ExecId: first.GetStarted().GetExecId()}
			require.NotEmpty(t, request.GetExecId())

			waitCtx, cancelWait := context.WithTimeout(ctx, 20*time.Millisecond)
			defer cancelWait()
			_, err = client.Wait(waitCtx, request)
			require.Equal(t, codes.DeadlineExceeded, status.Code(err))

			err = stream.Send(&v1.ExecRequest{
				Type: &v1.ExecRequest_StandardInput{
					StandardInput: &v1.IOChunk{Data: []byte("start\n")},
				},
			})
			require.NoError(t, err)
			require.NoError(t, stream.CloseSend())

			var waits errgroup.Group
			exits := make([]*v1.ExecResponse_Exit, 2)
			for i := range exits {
				waits.Go(func() error {
					var err error
					exits[i], err = client.Wait(ctx, request)
					return err
				})
			}
			require.NoError(t, waits.Wait())
			for _, exit := range exits {
				require.Equal(t, test.code, exit.GetCode())
			}

			var size int
			for {
				response, err := stream.Recv()
				require.NoError(t, err)
				if final := response.GetExit(); final != nil {
					require.Equal(t, test.code, final.GetCode())
					break
				}

				data := response.GetStandardError().GetData()
				require.NotEmpty(t, data)
				require.Equal(t, len(data), bytes.Count(data, []byte{0}))
				size += len(data)
			}
			require.Equal(t, outputBytes, size)
			_, err = stream.Recv()
			require.ErrorIs(t, err, io.EOF)

			_, err = client.Wait(ctx, request)
			require.Equal(t, codes.NotFound, status.Code(err))
		})
	}
}

func TestExecPreservesChildIOAfterParentExit(t *testing.T) {
	for _, mode := range []string{"without Wait", "with Wait"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			parentPIDPath := filepath.Join(dir, "parent-pid")
			childPIDPath := filepath.Join(dir, "child-pid")
			ctx, cancel := context.WithTimeout(t.Context(), execTestTimeout)
			defer cancel()

			rpc, stream, result := startExecTest(t, &v1.ExecRequest_Command{
				Name: execTestShell,
				Args: []string{"-c", `exec 3<&0; /bin/cat <&3 &
printf %d "$!" > "$CHILD_PID_FILE"
printf %d "$$" > "$PARENT_PID_FILE"
exit 17`},
				Env: map[string]string{
					"PARENT_PID_FILE": parentPIDPath,
					"CHILD_PID_FILE":  childPIDPath,
				},
				Interactive: true,
			}, func(stream *execTestStream) {
				stream.ctx = ctx
				stream.sendHook = func(*v1.ExecResponse) error { return ctx.Err() }
			})
			started := receiveExecResponse(t, stream).GetStarted()
			require.NotEmpty(t, started.GetExecId())

			childPID, err := waitForExecTestPID(childPIDPath)
			require.NoError(t, err)
			t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })

			if mode == "with Wait" {
				exit, err := rpc.Wait(ctx, &v1.WaitRequest{ExecId: started.GetExecId()})
				require.NoError(t, err)
				require.EqualValues(t, 17, exit.GetCode())
			} else {
				parentPID, err := waitForExecTestPID(parentPIDPath)
				require.NoError(t, err)
				require.Eventually(t, func() bool {
					return !execTestProcessRunning(t, parentPID)
				}, execTestTimeout, 10*time.Millisecond)
			}

			const input = "input after parent exit"
			stream.requests <- &v1.ExecRequest{
				Type: &v1.ExecRequest_StandardInput{
					StandardInput: &v1.IOChunk{Data: []byte(input)},
				},
			}

			var output []byte
			for len(output) < len(input) {
				response := receiveExecResponse(t, stream)
				require.Nil(t, response.GetExit())
				output = append(output, response.GetStandardOutput().GetData()...)
			}
			require.Equal(t, input, string(output))

			cancel()
			require.ErrorIs(t, receiveExecResult(t, result), context.Canceled)
			require.Eventually(t, func() bool {
				return !execTestProcessRunning(t, childPID)
			}, execTestTimeout, 10*time.Millisecond)
		})
	}
}

func TestWaitStopsSignalsAndCancellationClosesIO(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "child-pid")
	ctx, cancel := context.WithTimeout(t.Context(), execTestTimeout)
	defer cancel()

	rpc, stream, result := startExecTest(t, &v1.ExecRequest_Command{
		Name: execTestShell,
		Args: []string{"-c", `sleep 30 & printf %d "$!" > "$PID_FILE"; exit 17`},
		Env:  map[string]string{execTestPIDEnv: pidPath},
	}, func(stream *execTestStream) {
		stream.ctx = ctx
		stream.sendHook = func(*v1.ExecResponse) error { return ctx.Err() }
	})
	started := receiveExecResponse(t, stream).GetStarted()
	require.NotEmpty(t, started.GetExecId())

	childPID, err := waitForExecTestPID(pidPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })

	exit, err := rpc.Wait(ctx, &v1.WaitRequest{ExecId: started.GetExecId()})
	require.NoError(t, err)
	require.EqualValues(t, 17, exit.GetCode())

	_, err = rpc.Signal(ctx, &v1.SignalRequest{
		ExecId: started.GetExecId(),
		Signal: v1.SignalRequest_SIGNAL_SIGKILL,
	})
	require.NoError(t, err)
	require.True(t, execTestProcessRunning(t, childPID))

	cancel()
	require.ErrorIs(t, receiveExecResult(t, result), context.Canceled)
	require.True(t, execTestProcessRunning(t, childPID))
}

func TestWaitReturnsStdinErrorAfterExit(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "child-pid")
	rpc, stream, result := startExecTest(t, &v1.ExecRequest_Command{
		Name:        execTestShell,
		Args:        []string{"-c", `/bin/sh -c 'exec </dev/null; printf %d "$$" > "$PID_FILE"; exec sleep 30' & exit 17`},
		Env:         map[string]string{execTestPIDEnv: pidPath},
		Interactive: true,
	})
	started := receiveExecResponse(t, stream).GetStarted()
	require.NotNil(t, started)

	childPID, err := waitForExecTestPID(pidPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })

	ctx, cancel := context.WithTimeout(t.Context(), execTestTimeout)
	defer cancel()

	exit, err := rpc.Wait(ctx, &v1.WaitRequest{ExecId: started.GetExecId()})
	require.NoError(t, err)
	require.EqualValues(t, 17, exit.GetCode())

	stream.requests <- &v1.ExecRequest{
		Type: &v1.ExecRequest_StandardInput{
			StandardInput: &v1.IOChunk{Data: []byte("closed stdin")},
		},
	}
	require.ErrorIs(t, receiveExecResult(t, result), syscall.EPIPE)
	require.True(t, execTestProcessRunning(t, childPID))

	_, err = rpc.Wait(ctx, &v1.WaitRequest{ExecId: started.GetExecId()})
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestWaitCancellationClosesPTYAfterResize(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux keeps the PTY readable while a descendant holds the slave open")
	}

	pidPath := filepath.Join(t.TempDir(), "child-pid")
	ctx, cancel := context.WithTimeout(t.Context(), execTestTimeout)
	defer cancel()

	rpc, stream, result := startExecTest(t, &v1.ExecRequest_Command{
		Name: execTestShell,
		Args: []string{"-c", `trap '' HUP
stty -echo
printf ready
read start
stty size
sleep 30 & printf %d "$!" > "$PID_FILE"
exit 17`},
		Env:         map[string]string{execTestPIDEnv: pidPath},
		Interactive: true,
		Tty:         true,
	}, func(stream *execTestStream) {
		stream.ctx = ctx
		stream.sendHook = func(*v1.ExecResponse) error { return ctx.Err() }
	})
	started := receiveExecResponse(t, stream).GetStarted()
	require.NotNil(t, started)
	require.Equal(t, "ready", string(receiveExecResponse(t, stream).GetStandardOutput().GetData()))

	stream.requests <- &v1.ExecRequest{
		Type: &v1.ExecRequest_TerminalResize{
			TerminalResize: &v1.TerminalSize{Rows: 30, Cols: 100},
		},
	}
	stream.requests <- &v1.ExecRequest{
		Type: &v1.ExecRequest_StandardInput{
			StandardInput: &v1.IOChunk{Data: []byte("start\n")},
		},
	}
	require.Equal(t, "30 100\r\n", string(receiveExecResponse(t, stream).GetStandardOutput().GetData()))

	childPID, err := waitForExecTestPID(pidPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })

	exit, err := rpc.Wait(ctx, &v1.WaitRequest{ExecId: started.GetExecId()})
	require.NoError(t, err)
	require.EqualValues(t, 17, exit.GetCode())

	cancel()
	require.ErrorIs(t, receiveExecResult(t, result), context.Canceled)
	require.True(t, execTestProcessRunning(t, childPID))

	_, err = rpc.Wait(t.Context(), &v1.WaitRequest{ExecId: started.GetExecId()})
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestExecClosesStandardInputOnRequestStreamEOF(t *testing.T) {
	_, stream, result := startExecTest(t, &v1.ExecRequest_Command{
		Name:        "/bin/cat",
		Interactive: true,
	})
	require.NotNil(t, receiveExecResponse(t, stream).GetStarted())

	stream.requests <- &v1.ExecRequest{
		Type: &v1.ExecRequest_StandardInput{
			StandardInput: &v1.IOChunk{Data: []byte("hello")},
		},
	}
	close(stream.requests)

	response := receiveExecResponse(t, stream)
	require.Equal(t, []byte("hello"), response.GetStandardOutput().GetData())
	response = receiveExecResponse(t, stream)
	require.EqualValues(t, 0, response.GetExit().GetCode())
	require.NoError(t, receiveExecResult(t, result))
}

func TestExecClosesStandardInputOnEmptyChunk(t *testing.T) {
	_, stream, result := startExecTest(t, &v1.ExecRequest_Command{
		Name:        "/bin/cat",
		Interactive: true,
	})
	require.NotNil(t, receiveExecResponse(t, stream).GetStarted())

	stream.requests <- &v1.ExecRequest{
		Type: &v1.ExecRequest_StandardInput{
			StandardInput: &v1.IOChunk{Data: []byte("hello")},
		},
	}
	stream.requests <- &v1.ExecRequest{
		Type: &v1.ExecRequest_StandardInput{
			StandardInput: &v1.IOChunk{},
		},
	}

	response := receiveExecResponse(t, stream)
	require.Equal(t, []byte("hello"), response.GetStandardOutput().GetData())
	response = receiveExecResponse(t, stream)
	require.EqualValues(t, 0, response.GetExit().GetCode())
	require.NoError(t, receiveExecResult(t, result))
}

func TestExecReportsStartFailureBeforeStarted(t *testing.T) {
	tests := []struct {
		name    string
		command *v1.ExecRequest_Command
	}{
		{
			name: "missing executable",
			command: &v1.ExecRequest_Command{
				Name: "/definitely/missing/tart-guest-agent-test-command",
			},
		},
		{
			name: "missing workdir",
			command: &v1.ExecRequest_Command{
				Name:    execTestShell,
				Workdir: "/definitely/missing/tart-guest-agent-test-workdir",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, stream, result := startExecTest(t, test.command)
			response := receiveExecResponse(t, stream)
			require.Nil(t, response.GetStarted())
			require.EqualValues(t, execRuntimeFailureExitCode, response.GetExit().GetCode())
			require.NoError(t, receiveExecResult(t, result))
		})
	}
}

func TestExecSignalsProcess(t *testing.T) {
	tests := []struct {
		name   string
		signal v1.SignalRequest_Signal
		code   int32
		err    string
	}{
		{
			name:   "SIGTERM",
			signal: v1.SignalRequest_SIGNAL_SIGTERM,
			code:   int32(signalExitCodeOffset + syscall.SIGTERM),
		},
		{
			name:   "SIGKILL",
			signal: v1.SignalRequest_SIGNAL_SIGKILL,
			code:   int32(signalExitCodeOffset + syscall.SIGKILL),
		},
		{
			name:   "unsupported",
			signal: v1.SignalRequest_SIGNAL_UNSPECIFIED,
			err:    `unsupported exec signal "SIGNAL_UNSPECIFIED"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rpc, stream, result := startExecTest(t, &v1.ExecRequest_Command{
				Name: "/bin/sleep",
				Args: []string{"30"},
			})
			started := receiveExecResponse(t, stream).GetStarted()
			require.NotNil(t, started)

			_, err := rpc.Signal(context.Background(), &v1.SignalRequest{
				ExecId: started.GetExecId(),
				Signal: test.signal,
			})

			if test.err != "" {
				require.EqualError(t, err, test.err)
				_, err = rpc.Signal(context.Background(), &v1.SignalRequest{
					ExecId: started.GetExecId(),
					Signal: v1.SignalRequest_SIGNAL_SIGKILL,
				})
				require.NoError(t, err)
				receiveExecResponse(t, stream)
				require.NoError(t, receiveExecResult(t, result))

				return
			}
			require.NoError(t, err)

			response := receiveExecResponse(t, stream)
			require.NotNil(t, response.GetExit())
			require.Equal(t, test.code, response.GetExit().GetCode())
			require.NoError(t, receiveExecResult(t, result))
		})
	}
}

func TestExecSignalsProcessGroup(t *testing.T) {
	rpc, stream, result := startExecTest(t, &v1.ExecRequest_Command{
		Name: execTestShell,
		Args: []string{"-c", "sleep 30 & printf ready; wait"},
	})
	started := receiveExecResponse(t, stream).GetStarted()
	require.NotNil(t, started)
	require.Equal(t, []byte("ready"), receiveExecResponse(t, stream).GetStandardOutput().GetData())

	_, err := rpc.Signal(context.Background(), &v1.SignalRequest{
		ExecId: started.GetExecId(),
		Signal: v1.SignalRequest_SIGNAL_SIGTERM,
	})
	require.NoError(t, err)

	response := receiveExecResponse(t, stream)
	require.EqualValues(t, signalExitCodeOffset+syscall.SIGTERM, response.GetExit().GetCode())
	require.NoError(t, receiveExecResult(t, result))
}

func TestExecReapsProcessWhenStartedCannotBeSent(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "pid")
	sendErr := errors.New("failed to send Started")
	var processPID int

	_, _, result := startExecTest(t, &v1.ExecRequest_Command{
		Name: execTestShell,
		Args: []string{"-c", `printf %d "$$" > "$PID_FILE"; exec sleep 30`},
		Env:  map[string]string{execTestPIDEnv: pidPath},
	}, func(stream *execTestStream) {
		stream.sendHook = func(response *v1.ExecResponse) error {
			if response.GetStarted() == nil {
				return nil
			}

			var err error
			processPID, err = waitForExecTestPID(pidPath)
			if err != nil {
				return err
			}

			return sendErr
		}
	})

	require.ErrorIs(t, receiveExecResult(t, result), sendErr)
	require.ErrorIs(t, syscall.Kill(processPID, 0), syscall.ESRCH)
}

func startExecTest(
	t *testing.T,
	command *v1.ExecRequest_Command,
	configure ...func(*execTestStream),
) (*RPC, *execTestStream, <-chan error) {
	t.Helper()
	rpc, err := New(nil)
	require.NoError(t, err)
	return startExecTestWithRPC(t, rpc, command, configure...)
}

func startExecTestWithRPC(
	t *testing.T,
	rpc *RPC,
	command *v1.ExecRequest_Command,
	configure ...func(*execTestStream),
) (*RPC, *execTestStream, <-chan error) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream := newExecTestStream(ctx)
	for _, configureStream := range configure {
		configureStream(stream)
	}
	result := make(chan error, 1)
	go func() {
		result <- rpc.Exec(stream)
	}()
	stream.requests <- &v1.ExecRequest{
		Type: &v1.ExecRequest_Command_{Command: command},
	}
	return rpc, stream, result
}

func receiveExecResponse(t *testing.T, stream *execTestStream) *v1.ExecResponse {
	t.Helper()

	select {
	case response := <-stream.responses:
		return response
	case <-time.After(execTestTimeout):
		t.Fatal("timed out waiting for exec response")
		return nil
	}
}

func receiveExecResult(t *testing.T, result <-chan error) error {
	t.Helper()

	select {
	case err := <-result:
		return err
	case <-time.After(execTestTimeout):
		t.Fatal("timed out waiting for Exec to return")
		return nil
	}
}

func waitForExecTestPID(path string) (int, error) {
	deadline := time.Now().Add(execTestTimeout)
	for time.Now().Before(deadline) {
		//nolint:gosec // path is created under t.TempDir by the test
		data, err := os.ReadFile(path)
		if err == nil {
			return strconv.Atoi(string(data))
		}
		if !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}

		time.Sleep(10 * time.Millisecond)
	}

	return 0, context.DeadlineExceeded
}
