// Package child runs one locked-down helper process: it gets only the marker
// environment entry (never the parent's BOXSET_* settings or any secret), its own
// process group or job object so a stop takes every descendant with it, and a
// transcript that is drained without limit, retained up to a cap and redacted
// before anyone reads it.
package child

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/redact"
)

// The whole environment a child gets is Marker: it names the child in the
// process table, and `serve` refuses to run under it, so a child can never
// become a second server.
const (
	Name = "BOXSET_CHILD"
	// Marker is the one environment entry a child receives.
	Marker = Name + "=1"
	// Cap is the most transcript a child's output is retained as.
	Cap = 64 << 10
	// waitDelay bounds the wait for a child that does not exit after its group
	// was killed, or that leaves a pipe open.
	waitDelay = 5 * time.Second
)

// ErrJobUnavailable: the platform refused to put the child in the group or job
// that kills its descendants with it, so it was terminated instead of run
// unsupervised.
var ErrJobUnavailable = errors.New("child: child_job_unavailable")

var ErrCompletionUnknown = errors.New("child: child_completion_unknown")

type AttemptID [16]byte

type CompletionState string

const (
	TreeQuiescent     CompletionState = "tree_quiescent"
	CompletionUnknown CompletionState = "unknown"
)

type Completion struct {
	AttemptID AttemptID
	State     CompletionState
}

// Result is what a run ended with.
type Result struct {
	// Exit is the process's exit status; -1 when a signal ended it.
	Exit int
	// TimedOut reports that Timeout ended the run.
	TimedOut bool
	// Output is the drained transcript, at most Cap bytes, redacted.
	Output          string
	Completion      Completion
	cleanupDeadline time.Time
}

// Spec is one child to run.
type Spec struct {
	// Path is the program and Args its own arguments. Neither ever holds a
	// secret.
	Path string
	Args []string
	// Dir is the working directory; "" keeps the caller's.
	Dir string
	// Timeout bounds the whole run. A child that passes it is killed with its
	// descendants.
	Timeout   time.Duration
	AttemptID AttemptID
}

// Proc is one running child.
type Proc struct {
	ctx    context.Context
	cancel context.CancelFunc
	cmd    *exec.Cmd
	out    *capture
	done   chan struct{}
	result Result
	err    error
}

// Start starts spec in the background, with in as its standard input when it is
// not nil. A child that is already past its Timeout to be killed at once.
func Start(ctx context.Context, spec Spec, in io.Reader) (*Proc, error) {
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	var input *os.File
	if in != nil {
		var ok bool
		input, ok = in.(*os.File)
		if !ok || input == nil {
			return nil, ErrJobUnavailable
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return startManaged(ctx, spec, input)
}

func validateSpec(spec Spec) error {
	if spec.AttemptID == (AttemptID{}) || spec.Timeout <= 0 || spec.Path == "" || len(spec.Path) > 4096 || len(spec.Dir) > 4096 || len(spec.Args) > 128 {
		return ErrJobUnavailable
	}
	for _, value := range append([]string{spec.Path, spec.Dir}, spec.Args...) {
		if len(value) > 4096 || strings.ContainsRune(value, 0) {
			return ErrJobUnavailable
		}
	}
	return nil
}

// Wait returns the cached run result. Only a matching TreeQuiescent completion
// proves the managed tree has stopped; an uncertain observation returns unknown.
func (p *Proc) Wait() (Result, error) {
	<-p.done
	return p.result, p.err
}

// Stop cancels the run and joins its bounded cleanup. Wait retains the completion
// result, including unknown. Calling Stop after completion performs no signaling.
func (p *Proc) Stop() {
	select {
	case <-p.done:
		return
	default:
	}
	p.cancel()
	<-p.done
}

func (p *Proc) finish(result Result, err error) {
	result.Output = p.out.String()
	p.result, p.err = result, err
	p.cancel()
	close(p.done)
}

// Run runs spec to completion. The transcript of both streams is read to the end
// whatever its size, so the child never blocks on a full pipe; at most Cap bytes
// are retained, and they are redacted before they are returned.
func Run(ctx context.Context, spec Spec) (Result, error) {
	proc, err := Start(ctx, spec, nil)
	if err != nil {
		return Result{}, err
	}
	return proc.Wait()
}

// capture keeps the first limit bytes of a stream and counts the rest away, so
// the writer is always drained.
type capture struct {
	mu      sync.Mutex
	limit   int
	bytes   []byte
	dropped int
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := c.limit - len(c.bytes)
	if room <= 0 {
		c.dropped += len(p)
		return len(p), nil
	}
	kept := p
	if len(kept) > room {
		kept = kept[:room]
		c.dropped += len(p) - room
	}
	c.bytes = append(c.bytes, kept...)
	return len(p), nil
}

func (c *capture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return redact.String(string(c.bytes))
}
