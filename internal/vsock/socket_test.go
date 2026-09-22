//nolint:testpackage
package vsock

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const socketTestTimeout = 5 * time.Second

func TestMain(m *testing.M) {
	if path := os.Getenv("TART_GUEST_AGENT_TEST_SOCKET_PATH"); path != "" {
		if err := checkInheritedSockets(path); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func checkInheritedSockets(path string) error {
	for value := range strings.SplitSeq(os.Getenv("TART_GUEST_AGENT_TEST_SOCKET_FDS"), ",") {
		socketFD, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid socket descriptor %q: %w", value, err)
		}

		address, err := unix.Getsockname(socketFD)
		if err != nil {
			continue
		}
		if address, ok := address.(*unix.SockaddrUnix); ok && address.Name == path {
			return fmt.Errorf("socket descriptor %d was inherited", socketFD)
		}
	}
	return nil
}

func TestSocketDescriptorsNotInherited(t *testing.T) {
	// Keep the path below Darwin's Unix-domain socket path limit.
	dir, err := os.MkdirTemp("", "vsock-") //nolint:usetesting
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	path := filepath.Join(dir, "listener.sock")
	socketFD, err := newSocket(unix.AF_UNIX)
	require.NoError(t, err)
	file := os.NewFile(uintptr(socketFD), "test listener")
	t.Cleanup(func() { require.NoError(t, file.Close()) })

	require.NoError(t, unix.Bind(socketFD, &unix.SockaddrUnix{Name: path}))
	require.NoError(t, unix.Listen(socketFD, unix.SOMAXCONN))
	requireSocketFlags(t, socketFD)

	_, err = acceptSocket(file)
	require.ErrorIs(t, err, unix.EAGAIN)

	dialer := net.Dialer{Timeout: socketTestTimeout}
	client, err := dialer.DialContext(t.Context(), "unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	acceptedFD := acceptAfterFork(t, file)
	t.Cleanup(func() { require.NoError(t, unix.Close(acceptedFD)) })
	requireSocketFlags(t, acceptedFD)

	ctx, cancel := context.WithTimeout(t.Context(), socketTestTimeout)
	defer cancel()
	testExecutable, err := os.Executable()
	require.NoError(t, err)
	// #nosec G204 -- Re-execute the current test binary.
	cmd := exec.CommandContext(ctx, testExecutable)
	cmd.Env = append(os.Environ(),
		"TART_GUEST_AGENT_TEST_SOCKET_PATH="+path,
		fmt.Sprintf("TART_GUEST_AGENT_TEST_SOCKET_FDS=%d,%d", socketFD, acceptedFD),
	)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "child process: %s", output)
}

func acceptAfterFork(t *testing.T, file *os.File) int {
	t.Helper()

	type result struct {
		socketFD int
		err      error
	}
	accepted := make(chan result, 1)

	syscall.ForkLock.Lock()
	go func() {
		socketFD, err := acceptSocket(file)
		accepted <- result{socketFD: socketFD, err: err}
	}()

	select {
	case early := <-accepted:
		syscall.ForkLock.Unlock()
		if early.socketFD >= 0 {
			require.NoError(t, unix.Close(early.socketFD))
		}
		t.Fatalf("accept returned while ForkLock was held: %v", early.err)
	case <-time.After(100 * time.Millisecond):
		syscall.ForkLock.Unlock()
	}

	select {
	case accepted := <-accepted:
		require.NoError(t, accepted.err)
		return accepted.socketFD
	case <-time.After(socketTestTimeout):
		t.Fatal("accept did not finish after ForkLock was released")
		return -1
	}
}

func requireSocketFlags(t *testing.T, socketFD int) {
	t.Helper()

	flags, err := unix.FcntlInt(uintptr(socketFD), unix.F_GETFD, 0)
	require.NoError(t, err)
	require.NotZero(t, flags&unix.FD_CLOEXEC, "descriptor %d must be close-on-exec", socketFD)

	flags, err = unix.FcntlInt(uintptr(socketFD), unix.F_GETFL, 0)
	require.NoError(t, err)
	require.NotZero(t, flags&unix.O_NONBLOCK, "descriptor %d must be nonblocking", socketFD)
}
