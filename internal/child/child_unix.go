//go:build unix && !linux

package child

import (
	"context"
	"os"
	"os/exec"
	"syscall"
)

func startPlatformManaged(context.Context, Spec, *os.File) (*Proc, error) {
	return nil, ErrJobUnavailable
}

func prepareDiagnostic(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
