//nolint:testpackage // Exercise the shared accept implementation without a VSOCK device.
package vsock

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func newAcceptTestListener(t *testing.T) (*os.File, string) {
	t.Helper()

	// Keep the address below Darwin's Unix-domain socket path limit.
	dir, err := os.MkdirTemp("", "accept-") //nolint:usetesting
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	path := filepath.Join(dir, "socket")
	fd, err := newSocket(unix.AF_UNIX)
	require.NoError(t, err)
	file := os.NewFile(uintptr(fd), "accept test")
	t.Cleanup(func() { _ = file.Close() })
	require.NoError(t, unix.Bind(fd, &unix.SockaddrUnix{Name: path}))
	require.NoError(t, unix.Listen(fd, unix.SOMAXCONN))
	require.NoError(t, file.SetReadDeadline(time.Now().Add(socketTestTimeout)))

	return file, path
}

func TestAcceptSocketWaitsForReadDeadline(t *testing.T) {
	file, _ := newAcceptTestListener(t)
	require.NoError(t, file.SetReadDeadline(time.Now().Add(20*time.Millisecond)))

	fd, err := acceptSocket(file)
	if fd >= 0 {
		_ = unix.Close(fd)
	}
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)
}

func TestAcceptSocketWaitsForConnection(t *testing.T) {
	file, path := newAcceptTestListener(t)
	type result struct {
		fd  int
		err error
	}
	accepted := make(chan result, 1)
	go func() {
		fd, err := acceptSocket(file)
		accepted <- result{fd: fd, err: err}
	}()

	select {
	case early := <-accepted:
		if early.fd >= 0 {
			_ = unix.Close(early.fd)
		}
		t.Fatalf("accept returned before a connection arrived: %v", early.err)
	case <-time.After(20 * time.Millisecond):
	}

	var dialer net.Dialer
	dialer.Timeout = socketTestTimeout
	client, err := dialer.DialContext(t.Context(), "unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	select {
	case result := <-accepted:
		require.NoError(t, result.err)
		defer unix.Close(result.fd)
		requireSocketFlags(t, result.fd)
	case <-time.After(socketTestTimeout):
		t.Fatal("accept did not return after a connection arrived")
	}
}

func TestAcceptSocketUnblocksOnClose(t *testing.T) {
	file, _ := newAcceptTestListener(t)
	accepted := make(chan error, 1)
	go func() {
		fd, err := acceptSocket(file)
		if fd >= 0 {
			_ = unix.Close(fd)
		}
		accepted <- err
	}()

	select {
	case err := <-accepted:
		t.Fatalf("accept returned before close: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	require.NoError(t, file.Close())
	select {
	case err := <-accepted:
		require.Error(t, err)
	case <-time.After(socketTestTimeout):
		t.Fatal("close did not interrupt accept")
	}
}
