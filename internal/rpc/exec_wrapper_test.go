//nolint:testpackage
package rpc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cirruslabs/tart-guest-agent/pkg/v1"
	"github.com/stretchr/testify/require"
)

func TestValidateExecWrapper(t *testing.T) {
	notExecutable := filepath.Join(t.TempDir(), "not-executable")
	require.NoError(t, os.WriteFile(notExecutable, nil, 0o600))

	for _, argv := range [][]string{
		{""}, {"sh"}, {"/missing/tart-exec-wrapper"}, {t.TempDir()},
		{notExecutable}, {execTestShell, "bad\x00argument"},
	} {
		require.Error(t, ValidateExecWrapper(argv), "%q", argv)
		_, err := New(nil, argv...)
		require.Error(t, err)
	}

	require.NoError(t, ValidateExecWrapper(nil))
	require.NoError(t, ValidateExecWrapper([]string{execTestShell, "", "literal,$value"}))
}

func TestValidateExecWrapperRequiresApplicableExecutePermission(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can execute a file with any execute bit set")
	}

	path := filepath.Join(t.TempDir(), "other-executable")
	contents, err := os.ReadFile("/usr/bin/true")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, contents, 0o600)) // #nosec G703 -- Destination is inside t.TempDir.
	require.NoError(t, os.Chmod(path, 0o001))               // #nosec G302 -- Tests inapplicable execute permission.
	// The current user owns this file, so its other-execute bit does not apply.
	// #nosec G204 -- Path is an owned permission-test fixture.
	require.ErrorIs(t, exec.CommandContext(t.Context(), path).Run(), os.ErrPermission)
	require.ErrorIs(t, ValidateExecWrapper([]string{path}), os.ErrPermission)
	_, err = New(nil, path)
	require.ErrorIs(t, err, os.ErrPermission)
}

func TestExecWrapperPreservesArgumentsAndOverrides(t *testing.T) {
	workdir := t.TempDir()
	canonicalWorkdir, err := filepath.EvalSymlinks(workdir)
	require.NoError(t, err)
	wrapper := writeExecWrapper(t, `printf '%s\n' "$1"
shift
exec "$@"
`)
	prefix := []string{wrapper, "fixed,argument $HOME; $(false)"}
	rpc, err := New(nil, prefix...)
	require.NoError(t, err)
	prefix[0] = "/missing/changed-after-startup"
	prefix[1] = "changed-after-startup"

	_, stream, result := startExecTestWithRPC(t, rpc, &v1.ExecRequest_Command{
		Name:    execTestShell,
		Args:    []string{"-c", `printf '%s\n' "$PWD" "$WRAPPER_TEST_VALUE" "$1"; id -u`, "test", "literal; $(false)"},
		Env:     map[string]string{"WRAPPER_TEST_VALUE": "value with spaces"},
		Workdir: workdir,
		User:    strconv.Itoa(os.Getuid()),
	})
	output, exitCode := collectExecWrapperResult(t, stream)
	require.Zero(t, exitCode)
	require.Equal(t, fmt.Sprintf("fixed,argument $HOME; $(false)\n%s\nvalue with spaces\nliteral; $(false)\n%d\n",
		canonicalWorkdir, os.Getuid()), output)
	require.NoError(t, receiveExecResult(t, result))
}

func TestExecWrapperDeniesEveryExecutionMode(t *testing.T) {
	const detachedMode = "detached"
	for _, mode := range []string{"normal", "interactive", "pty", detachedMode} {
		t.Run(mode, func(t *testing.T) {
			directory := t.TempDir()
			receipt := filepath.Join(directory, "wrapper-ran")
			target := filepath.Join(directory, "target-ran")
			wrapper := writeExecWrapper(t, `printf denied > "$WRAPPER_RECEIPT"
exit 86
`)
			rpc, err := New(nil, wrapper)
			require.NoError(t, err)
			_, stream, result := startExecTestWithRPC(t, rpc, &v1.ExecRequest_Command{
				Name:        execTestShell,
				Args:        []string{"-c", `printf unrestricted > "$TARGET_RECEIPT"`},
				Env:         map[string]string{"WRAPPER_RECEIPT": receipt, "TARGET_RECEIPT": target},
				Interactive: mode == "interactive",
				Tty:         mode == "pty",
				Detach:      mode == detachedMode,
			})
			_, exitCode := collectExecWrapperResult(t, stream)
			if mode == detachedMode {
				require.Zero(t, exitCode) // Detached success acknowledges process creation.
			} else {
				require.EqualValues(t, 86, exitCode)
			}
			require.NoError(t, receiveExecResult(t, result))
			require.Eventually(t, func() bool { _, err := os.Stat(receipt); return err == nil },
				execTestTimeout, 10*time.Millisecond)
			_, err = os.Stat(target)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestExecWrapperDisappearingDoesNotRunTarget(t *testing.T) {
	wrapper := writeExecWrapper(t, "exec \"$@\"\n")
	rpc, err := New(nil, wrapper)
	require.NoError(t, err)
	require.NoError(t, os.Remove(wrapper))

	_, stream, result := startExecTestWithRPC(t, rpc, &v1.ExecRequest_Command{Name: "/usr/bin/true"})
	response := receiveExecResponse(t, stream)
	require.Nil(t, response.GetStarted())
	require.EqualValues(t, execRuntimeFailureExitCode, response.GetExit().GetCode())
	require.NoError(t, receiveExecResult(t, result))
}

func TestExecWrapperSignalsProcessGroup(t *testing.T) {
	for _, tty := range []bool{false, true} {
		t.Run(fmt.Sprintf("tty=%t", tty), func(t *testing.T) {
			wrapper := writeExecWrapper(t, "exec \"$@\"\n")
			rpc, err := New(nil, wrapper)
			require.NoError(t, err)
			_, stream, result := startExecTestWithRPC(t, rpc, &v1.ExecRequest_Command{
				Name: execTestShell,
				Args: []string{"-c", "sleep 30 & printf ready; wait"},
				Tty:  tty,
			})
			started := receiveExecResponse(t, stream).GetStarted()
			require.NotNil(t, started)
			require.Equal(t, []byte("ready"), receiveExecResponse(t, stream).GetStandardOutput().GetData())

			_, err = rpc.Signal(context.Background(), &v1.SignalRequest{
				ExecId: started.GetExecId(),
				Signal: v1.SignalRequest_SIGNAL_SIGTERM,
			})
			require.NoError(t, err)
			response := receiveExecResponse(t, stream)
			require.EqualValues(t, signalExitCodeOffset+syscall.SIGTERM, response.GetExit().GetCode())
			require.NoError(t, receiveExecResult(t, result))
		})
	}
}

func writeExecWrapper(t *testing.T, body string) string {
	path := filepath.Join(t.TempDir(), "wrapper")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o600))
	require.NoError(t, os.Chmod(path, 0o700)) // #nosec G302 -- This fixture must be executable.
	return path
}

func collectExecWrapperResult(t *testing.T, stream *execTestStream) (string, int32) {
	require.NotNil(t, receiveExecResponse(t, stream).GetStarted())
	var output strings.Builder
	for {
		response := receiveExecResponse(t, stream)
		switch value := response.GetType().(type) {
		case *v1.ExecResponse_StandardOutput:
			output.Write(value.StandardOutput.GetData())
		case *v1.ExecResponse_StandardError:
			output.Write(value.StandardError.GetData())
		case *v1.ExecResponse_Exit_:
			return output.String(), value.Exit.GetCode()
		default:
			t.Fatalf("unexpected exec response %T", value)
		}
	}
}
