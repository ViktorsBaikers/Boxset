//go:build !linux

package child

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"

	"github.com/ViktorsBaikers/Boxset/internal/logs"
)

func PrepareManagedOwner() error { return nil }

func startManaged(ctx context.Context, spec Spec, input *os.File) (*Proc, error) {
	return startPlatformManaged(ctx, spec, input)
}

func Enter(args []string) (bool, int) {
	if len(args) == 0 || args[0] != "__boxset-diagnostic" {
		return false, 0
	}
	if len(args) != 3 || args[1] != "1" || args[2] == "" || len(args[2]) > 4096 || strings.ContainsRune(args[2], 0) || len(os.Environ()) != 1 || os.Getenv(Name) != "1" {
		return true, 2
	}
	if logs.Monitor(args[2], os.Stdin) != nil {
		return true, 3
	}
	return true, 0
}

func startCrashDiagnostic(ctx context.Context, dir string, input *os.File) (*Proc, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, ErrJobUnavailable
	}
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, self, "__boxset-diagnostic", "1", dir)
	cmd.Env = []string{Marker}
	cmd.Stdin = input
	out := &capture{limit: Cap}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = waitDelay
	prepareDiagnostic(cmd)
	if cmd.Start() != nil {
		cancel()
		return nil, ErrJobUnavailable
	}
	p := &Proc{ctx: ctx, cancel: cancel, cmd: cmd, out: out, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		res := Result{}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			res.Exit = exit.ExitCode()
			err = nil
		}
		p.finish(res, err)
	}()
	return p, nil
}
