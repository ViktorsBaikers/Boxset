// Package logs owns the server's retained output: the rotating file that never
// holds more than its per-file cap, the in-memory ring the server's logger also
// keeps, and the crash monitor that publishes a bounded, redacted record of a
// crashed server only after that server has ended.
package logs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/bounded"
)

const (
	// FileCap is the most one rotating file ever holds. A write that would pass
	// it rotates first.
	FileCap = 8 << 20
	// CrashCap is the most a published crash record ever holds.
	CrashCap = 1 << 20
)

// RingHandler keeps the server's log in memory as well as writing it on: every
// record it receives is formatted into a per-record buffer, retained in a ring
// capped by bounded.LogRingCap and bounded.LogRingAge, and forwarded unchanged.
// Nothing reads the ring yet, and nothing but a redactor may wrap it: what it
// retains is what it was given.
type RingHandler struct {
	next slog.Handler
	ring *bounded.Ring
}

// NewRingHandler wraps next with the log ring the server retains.
func NewRingHandler(next slog.Handler) *RingHandler { return newRingHandler(next, time.Now) }

// newRingHandler builds the same handler on an injected clock, so a case can
// reach the age cap without waiting it out.
func newRingHandler(next slog.Handler, now func() time.Time) *RingHandler {
	return &RingHandler{next: next, ring: bounded.NewRingAt(bounded.LogRingCap, bounded.LogRingAge, now)}
}

// Ring is the ring the handler retains records in.
func (h *RingHandler) Ring() *bounded.Ring { return h.ring }

// Enabled reports what the wrapped handler accepts.
func (h *RingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle retains r, then forwards it unchanged. A capped buffer never refuses a
// write, so the wrapped handler's error is the only one there is to report.
func (h *RingHandler) Handle(ctx context.Context, r slog.Record) error {
	buf := cappedBuffer{max: bounded.LogRingCap}
	_ = slog.NewTextHandler(&buf, nil).Handle(ctx, r)
	h.ring.Append(buf.data)
	return h.next.Handle(ctx, r)
}

func (h *RingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &RingHandler{next: h.next.WithAttrs(attrs), ring: h.ring}
}

func (h *RingHandler) WithGroup(name string) slog.Handler {
	return &RingHandler{next: h.next.WithGroup(name), ring: h.ring}
}

// cappedBuffer collects at most max bytes of one formatted record and drops the
// rest, so what a single oversized record costs is bounded by the ring's cap.
type cappedBuffer struct {
	data []byte
	max  int
}

func (w *cappedBuffer) Write(p []byte) (int, error) {
	if room := w.max - len(w.data); room > 0 {
		w.data = append(w.data, p[:min(len(p), room)]...)
	}
	return len(p), nil
}

// DirName is the directory under the data directory that holds both.
const DirName = "logs"

// ErrLogDir: the log directory cannot be created or opened.
var ErrLogDir = errors.New("logs: the log directory is unavailable")

// Rotating is the server's own log: <dir>/boxset.log, rotated to boxset.log.1
// when the next write would pass FileCap. Neither file ever holds more than
// FileCap, at any moment, so a burst of any size costs at most 2xFileCap.
type Rotating struct {
	mu   sync.Mutex
	path string
	file *os.File
	size int
}

// Open opens the rotating log of the data directory, creating logs/ when it is
// absent.
func Open(dir string) (*Rotating, error) {
	root := filepath.Join(dir, DirName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLogDir, err)
	}
	r := &Rotating{path: filepath.Join(root, "boxset.log")}
	file, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLogDir, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("%w: %v", ErrLogDir, err)
	}
	r.file = file
	r.size = int(info.Size())
	return r, nil
}

// Path is the file the rotating log writes.
func (r *Rotating) Path() string { return r.path }

// Write appends p, rotating as often as it must so that no file passes
// FileCap. It never returns a short write: a record too large for one file is
// split across the rotation, and a write that cannot be stored is reported as
// an error so its caller can log elsewhere.
func (r *Rotating) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	written := 0
	for len(p) > 0 {
		if r.size >= FileCap {
			if err := r.rotate(); err != nil {
				return written, err
			}
		}
		room := FileCap - r.size
		chunk := p
		if len(chunk) > room {
			chunk = chunk[:room]
		}
		n, err := r.file.Write(chunk)
		r.size += n
		written += n
		if err != nil {
			return written, err
		}
		p = p[n:]
	}
	return written, nil
}

// Close closes the file. A stop that cannot close it reports why.
func (r *Rotating) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}

// rotate moves the current file to boxset.log.1, replacing any earlier rotation.
// Windows refuses a rename over an existing name, so the older file is removed
// first here too: one code path, one behaviour.
func (r *Rotating) rotate() error {
	if err := r.file.Close(); err != nil {
		return err
	}
	r.file = nil
	rotated := r.path + ".1"
	if err := os.Remove(rotated); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(r.path, rotated); err != nil {
		return err
	}
	file, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	r.file = file
	r.size = 0
	return nil
}
