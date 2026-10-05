//nolint:testpackage // Reuse the in-process RPC stream to exercise detached children.
package rpc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cirruslabs/tart-guest-agent/pkg/v1"
	"github.com/stretchr/testify/require"
)

func TestDetachedExecReapsFinishedChildren(t *testing.T) {
	for _, sendFails := range []bool{false, true} {
		for _, exitCode := range []int{0, 7} {
			t.Run(strconv.FormatBool(sendFails)+"/"+strconv.Itoa(exitCode), func(t *testing.T) {
				directory := t.TempDir()
				pidFile, gate := filepath.Join(directory, "pid"), filepath.Join(directory, "exit")
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				stream := newExecTestStream(ctx)
				sendError := errors.New("synthetic started-response failure")
				if sendFails {
					stream.sendHook = func(*v1.ExecResponse) error { return sendError }
				}
				var command v1.ExecRequest_Command
				command.Name = execTestShell
				command.Detach = true
				command.Args = []string{"-c", `
printf '%s' "$$" > "$PID_FILE"
while [ ! -f "$EXIT_GATE" ]; do sleep 0.01; done
printf output
printf error >&2
exit "$EXIT_CODE"
`}
				command.Env = map[string]string{
					"PID_FILE": pidFile, "EXIT_GATE": gate, "EXIT_CODE": strconv.Itoa(exitCode),
				}
				stream.requests <- &v1.ExecRequest{Type: &v1.ExecRequest_Command_{Command: &command}}
				server, err := New(nil)
				require.NoError(t, err)
				result := make(chan error, 1)
				go func() { result <- server.Exec(stream) }()
				var pid int
				require.Eventually(t, func() bool {
					data, err := os.ReadFile(pidFile) // #nosec G304 -- File belongs to this test's temporary directory.
					if err != nil {
						return false
					}
					pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
					return err == nil && pid > 0
				}, execTestTimeout, 5*time.Millisecond)
				t.Cleanup(func() {
					// Reap only this test's owned child after a failing baseline run.
					var status syscall.WaitStatus
					found, waitErr := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
					if waitErr == nil && found == 0 {
						_ = syscall.Kill(-pid, syscall.SIGKILL)
						_, _ = syscall.Wait4(pid, &status, 0, nil)
					}
				})
				if sendFails {
					require.ErrorIs(t, receiveExecResult(t, result), sendError)
				} else {
					require.NotNil(t, receiveExecResponse(t, stream).GetStarted())
					require.Zero(t, receiveExecResponse(t, stream).GetExit().GetCode())
					require.NoError(t, receiveExecResult(t, result))
				}
				// Detach still acknowledges creation before exit and survives stream cancellation.
				cancel()
				require.NoError(t, syscall.Kill(pid, 0))
				require.NoError(t, os.WriteFile(gate, nil, 0600))
				require.Eventually(t, func() bool {
					return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
				}, execTestTimeout, 5*time.Millisecond, "the completed child must be reaped, not left as a zombie")
			})
		}
	}
}
