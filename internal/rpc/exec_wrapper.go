package rpc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// ValidateExecWrapper checks the administrator-configured argv prefix.
// The wrapper itself is responsible for performing its work and executing the
// appended command. No request can change this prefix.
func ValidateExecWrapper(argv []string) error {
	if len(argv) == 0 {
		return nil
	}

	if !filepath.IsAbs(argv[0]) {
		return errors.New("exec wrapper executable must be an absolute path")
	}

	for _, arg := range argv {
		if strings.ContainsRune(arg, '\x00') {
			return errors.New("exec wrapper arguments must not contain NUL")
		}
	}

	info, err := os.Stat(argv[0])
	if err != nil {
		return fmt.Errorf("invalid exec wrapper executable: %w", err)
	}

	if !info.Mode().IsRegular() {
		return errors.New("exec wrapper must name an executable file")
	}
	if err := unix.Faccessat(unix.AT_FDCWD, argv[0], unix.X_OK, unix.AT_EACCESS); err != nil {
		return fmt.Errorf("exec wrapper is not executable by the guest agent: %w", err)
	}

	return nil
}

func (rpc *RPC) execCommand(ctx context.Context, name string, args []string) *exec.Cmd {
	if len(rpc.execWrapper) == 0 {
		return exec.CommandContext(ctx, name, args...) // #nosec G204 -- Executing RPC commands is this service's purpose.
	}

	wrappedArgs := make([]string, 0, len(rpc.execWrapper)+len(args))
	wrappedArgs = append(wrappedArgs, rpc.execWrapper[1:]...)
	wrappedArgs = append(wrappedArgs, name)
	wrappedArgs = append(wrappedArgs, args...)

	// #nosec G204 -- Fixed, validated administrator prefix.
	return exec.CommandContext(ctx, rpc.execWrapper[0], wrappedArgs...)
}
