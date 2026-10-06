//nolint:testpackage // Exercise the in-process stream and owned subprocess registry.
package rpc

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/cirruslabs/tart-guest-agent/pkg/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

const outputFailurePIDEnv = "PID_FILE"

func TestExecReapsCommandAfterOutputSendFailure(t *testing.T) {
	for _, channel := range []string{"stdout", "stderr", "tty"} {
		t.Run(channel, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pid")
			sendError := errors.New("synthetic output delivery failure")
			failed := make(chan struct{})
			redirect := ""
			if channel == "stderr" {
				redirect = " >&2"
			}
			var command v1.ExecRequest_Command
			command.Name = execTestShell
			command.Args = []string{"-c", `printf %d "$$" > "$PID_FILE"; printf synthetic` + redirect + `; exec sleep 30`}
			command.Env = map[string]string{outputFailurePIDEnv: pidFile}
			command.Tty = channel == "tty"
			_, stream, result := startExecTest(t, &command, func(stream *execTestStream) {
				stream.sendHook = func(response *v1.ExecResponse) error {
					if response.GetStandardOutput() != nil || response.GetStandardError() != nil {
						close(failed)
						return sendError
					}
					return nil
				}
			})
			close(stream.requests)
			require.NotNil(t, receiveExecResponse(t, stream).GetStarted())
			select {
			case <-failed:
			case <-time.After(execTestTimeout):
				t.Fatal("the output failure was not reached")
			}
			pid, err := waitForExecTestPID(pidFile)
			require.NoError(t, err)
			returned := false
			defer func() {
				if !returned {
					// Only this test's own process group is terminated in a failing baseline.
					_ = syscall.Kill(-pid, syscall.SIGKILL)
					select {
					case <-result:
					case <-time.After(execTestTimeout):
						t.Error("owned child did not finish after cleanup")
					}
				}
			}()
			require.NoError(t, stream.Context().Err(), "the RPC context has not been cancelled")
			select {
			case err := <-result:
				returned = true
				require.ErrorIs(t, err, sendError)
				require.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH)
			case <-time.After(time.Second):
				t.Fatal("output delivery failed but Exec is still waiting for the live child")
			}
		})
	}
}

func TestExecGRPCOutputLimitDoesNotKeepCommandAlive(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	agent, err := New(listener)
	require.NoError(t, err)
	agent.grpcServer.Stop()
	agent.grpcServer = grpc.NewServer(grpc.MaxSendMsgSize(128))
	v1.RegisterAgentServer(agent.grpcServer, agent)
	serve := make(chan error, 1)
	go func() { serve <- agent.grpcServer.Serve(listener) }()
	defer func() { agent.grpcServer.Stop(); <-serve; _ = listener.Close() }()
	connection, err := grpc.NewClient("passthrough:///synthetic-output-limit",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	require.NoError(t, err)
	defer connection.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	stream, err := v1.NewAgentClient(connection).Exec(ctx)
	require.NoError(t, err)
	pidFile := filepath.Join(t.TempDir(), "pid")
	var command v1.ExecRequest_Command
	command.Name = execTestShell
	command.Args = []string{"-c", `printf %d "$$" > "$PID_FILE"; printf '%0512d' 1; exec sleep 30`}
	command.Env = map[string]string{outputFailurePIDEnv: pidFile}
	require.NoError(t, stream.Send(&v1.ExecRequest{Type: &v1.ExecRequest_Command_{Command: &command}}))
	require.NoError(t, stream.CloseSend())
	response, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, response.GetStarted())
	pid, err := waitForExecTestPID(pidFile)
	require.NoError(t, err)
	defer func() {
		cancel()
		require.Eventually(t, func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) },
			execTestTimeout, 10*time.Millisecond)
	}()
	_, err = stream.Recv()
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.NoError(t, ctx.Err(), "the server must fail before the caller deadline")
	require.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH)
}
