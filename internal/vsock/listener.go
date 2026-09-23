package vsock

import (
	"fmt"
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

type listener struct {
	file *os.File
	port uint32
}

func Listen(port uint32) (net.Listener, error) {
	socketFD, err := newSocket(unix.AF_VSOCK)
	if err != nil {
		return nil, err
	}

	if err := unix.Bind(socketFD, &unix.SockaddrVM{
		CID:  unix.VMADDR_CID_ANY,
		Port: port,
	}); err != nil {
		_ = unix.Close(socketFD)
		return nil, err
	}

	if err := unix.Listen(socketFD, unix.SOMAXCONN); err != nil {
		_ = unix.Close(socketFD)
		return nil, err
	}

	return &listener{
		file: os.NewFile(uintptr(socketFD), "vsock"),
		port: port,
	}, nil
}

func (listener *listener) Accept() (net.Conn, error) {
	socketFD, err := acceptSocket(listener.file)
	if err != nil {
		return nil, err
	}

	peerName, err := unix.Getpeername(socketFD)
	if err != nil {
		_ = unix.Close(socketFD)
		return nil, fmt.Errorf("failed to get a peer name for an AF_VSOCK connection %w", err)
	}

	peerNameVM, ok := peerName.(*unix.SockaddrVM)
	if !ok {
		_ = unix.Close(socketFD)
		return nil, fmt.Errorf("accepted a non-AF_VSOCK connection on an AF_VSOCK socket")
	}

	return &conn{
		file:       os.NewFile(uintptr(socketFD), "vsock"),
		localPort:  listener.port,
		remotePort: peerNameVM.Port,
	}, nil
}

func (listener *listener) Addr() net.Addr {
	return &addr{port: listener.port}
}

func (listener *listener) Close() error {
	return listener.file.Close()
}

func newSocket(domain int) (int, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()

	socketFD, err := unix.Socket(domain, unix.SOCK_STREAM, 0)
	if err != nil {
		return -1, err
	}
	unix.CloseOnExec(socketFD)

	if err := unix.SetNonblock(socketFD, true); err != nil {
		_ = unix.Close(socketFD)
		return -1, err
	}
	return socketFD, nil
}

func acceptSocket(file *os.File) (int, error) {
	raw, err := file.SyscallConn()
	if err != nil {
		return -1, err
	}

	var socketFD int
	var acceptErr error
	err = raw.Control(func(listenerFD uintptr) {
		syscall.ForkLock.RLock()
		defer syscall.ForkLock.RUnlock()

		socketFD, _, acceptErr = unix.Accept(int(listenerFD))
		if acceptErr != nil {
			return
		}
		unix.CloseOnExec(socketFD)
	})
	if err != nil {
		return -1, err
	}
	if acceptErr != nil {
		return -1, acceptErr
	}
	if err := unix.SetNonblock(socketFD, true); err != nil {
		_ = unix.Close(socketFD)
		return -1, err
	}
	return socketFD, nil
}
