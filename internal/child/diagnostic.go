package child

import (
	"context"
	"os"
)

// StartCrashMonitor starts only the fixed same-binary EOF diagnostic.
func StartCrashMonitor(ctx context.Context, dir string, input *os.File) (*Proc, error) {
	if dir == "" || len(dir) > 4096 || input == nil {
		return nil, ErrJobUnavailable
	}
	return startCrashDiagnostic(ctx, dir, input)
}
