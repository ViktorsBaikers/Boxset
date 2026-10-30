package events

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ViktorsBaikers/Boxset/internal/auth"
)

// heartbeat is one idle proof: a comment line, which carries no data and cannot
// be mistaken for a frame.
var heartbeat = []byte(": hb\n\n")

// Stream is one admitted subscriber. The hub hands frames to its queue without
// waiting; this stream's own goroutine revalidates the credential before every
// frame, writes it under the frame deadline and closes when authority, the queue
// or the drain says so.
type Stream struct {
	hub       *Hub
	principal auth.Principal
	ref       auth.CredentialRef
	cap       string // its counter in the hub's stream cap
	user      int64
	watermark int64

	mu         sync.Mutex
	replay     [][]byte
	resync     string // a cursor the hub cannot honour; sent after hello
	queue      [][]byte
	bytes      int
	lastQueued int64
	overflowAt int64
	halted     bool
	draining   bool
	notify     chan struct{}
}

// reaches reports whether the stream's accepted rights deliver right. The
// system state reaches every authorized reader.
func (s *Stream) reaches(right string) bool {
	return right == "" || s.principal.Has(right)
}

// signal wakes the stream's goroutine without waiting for it.
func (s *Stream) signal() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// enqueue accepts one encoded frame, or records that the queue overflowed: the
// frame is never kept, and the stream will announce resync{overflow} with the
// watermark of the last frame it did accept.
func (s *Stream) enqueue(seq int64, frame []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.halted || s.draining {
		return
	}
	// Once a frame has been refused the stream ends at the announced watermark:
	// a frame accepted after it could only reach the wire behind that watermark.
	if s.overflowAt != 0 {
		return
	}
	if len(s.queue) >= queueFrames || s.bytes+len(frame) > queueBytes {
		s.overflowLocked(s.lastQueued)
		s.signal()
		return
	}
	s.queue = append(s.queue, frame)
	s.bytes += len(frame)
	s.lastQueued = seq
	s.signal()
}

// overflow records that seq cannot be represented: the stream announces the
// recovery watermark once its earlier queued frames are delivered, then closes.
func (s *Stream) overflow(seq int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.overflowLocked(seq)
	s.signal()
}

func (s *Stream) overflowLocked(seq int64) {
	if seq > s.overflowAt {
		s.overflowAt = seq
	}
}

// stop ends the stream before another frame: the principal's authority changed.
func (s *Stream) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.halted = true
	s.signal()
}

// drain lets the stream deliver what is already queued and then end.
func (s *Stream) drain() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.draining = true
	s.signal()
}

// pop hands the next queued frame to the stream's goroutine. lastQueued stays the
// last frame the queue accepted: it is the watermark an overflow announces, and a
// frame the peer has already been sent is never the bound of that.
func (s *Stream) pop() ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return nil, false
	}
	next := s.queue[0]
	s.queue = s.queue[1:]
	s.bytes -= len(next)
	return next, true
}

func (s *Stream) state() (halted, draining bool, overflow int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.halted, s.draining, s.overflowAt
}

// Serve writes the stream's frames until it closes: hello first, then the
// cursor's replay or its refusal, then live frames and heartbeats. Every frame
// and heartbeat is preceded by the one current-authority decision; a frame the
// credential no longer authorizes is never written.
func (s *Stream) Serve(ctx huma.Context) {
	defer s.hub.finished(s)
	writer, ok := ctx.BodyWriter().(http.ResponseWriter)
	if !ok {
		return
	}
	control := http.NewResponseController(writer)
	ctx.SetHeader("Content-Type", "text/event-stream")
	ctx.SetHeader("Cache-Control", "no-store")
	ctx.SetStatus(http.StatusOK)

	// The first decision arms this stream's own expiry timer and refuses a
	// credential that expired between the guard and the stream.
	expires, err := s.current(ctx.Context())
	if err != nil {
		return
	}
	if err := s.write(ctx, control, s.hello()); err != nil {
		return
	}
	if s.resync != "" {
		// The cursor cannot be honoured: the client refetches its snapshot and
		// opens a new stream from the recovery watermark.
		_ = s.write(ctx, control, resyncFrame(s.hub, s.watermark, s.resync))
		return
	}
	for _, frame := range s.replay {
		// The drain or the principal may have stopped this stream between frames:
		// a halt ends the replay rather than resting on the per-frame write bound.
		if halted, _, _ := s.state(); halted {
			return
		}
		if err := s.write(ctx, control, frame); err != nil {
			return
		}
	}

	every := s.hub.cfg.Heartbeat
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	var expiry <-chan time.Time
	if !expires.IsZero() {
		timer := time.NewTimer(time.Until(expires))
		defer timer.Stop()
		expiry = timer.C
	}
	for {
		if halted, _, _ := s.state(); halted {
			return
		}
		if frame, ok := s.pop(); ok {
			if err := s.write(ctx, control, frame); err != nil {
				return
			}
			continue
		}
		halted, draining, overflow := s.state()
		if halted || draining {
			return
		}
		if overflow != 0 {
			_ = s.write(ctx, control, resyncFrame(s.hub, overflow, "overflow"))
			return
		}
		select {
		case <-ctx.Context().Done():
			return
		case <-s.notify:
		case <-ticker.C:
			if err := s.write(ctx, control, heartbeat); err != nil {
				return
			}
		case <-expiry:
			return
		}
	}
}

// hello is the first frame: the watermark, and the identity the client confirms
// its tab against.
func (s *Stream) hello() []byte {
	rights := append([]string(nil), s.principal.Rights...)
	payload, _ := json.Marshal(EventHello{
		Watermark:    s.watermark,
		InstallID:    s.hub.cfg.InstallID,
		RestoreEpoch: s.hub.cfg.RestoreEpoch,
		UserID:       s.ref.User(),
		Credential:   s.ref.Credential(),
		Rights:       rights,
	})
	return encodeFrame(s.hub.frameID(s.watermark), "hello", payload)
}

func resyncFrame(h *Hub, watermark int64, reason string) []byte {
	payload, _ := json.Marshal(EventResync{Watermark: watermark, Reason: reason})
	return encodeFrame(h.frameID(watermark), "resync", payload)
}

// current re-runs the one current-authority decision for this stream's
// provenance under the frame deadline. It runs on the stream's own goroutine and
// never under the hub mutex, and writes nothing.
func (s *Stream) current(ctx context.Context) (time.Time, error) {
	check, cancel := context.WithTimeout(ctx, frameDeadline)
	defer cancel()
	_, expires, err := s.hub.cfg.Revalidate(check, s.ref)
	return expires, err
}

// write revalidates, writes and flushes one frame inside the frame deadline: the
// deadline is armed first so a stalled lookup, not only a stalled write, is
// bounded by it, and a stalled client is bounded by it rather than by the
// stream's life.
func (s *Stream) write(ctx huma.Context, control *http.ResponseController, frame []byte) error {
	_ = control.SetWriteDeadline(time.Now().Add(frameDeadline))
	if _, err := s.current(ctx.Context()); err != nil {
		s.hub.log("stream_closed", "reason", "credential_invalid", "credential", s.ref.Credential())
		return err
	}
	if _, err := ctx.BodyWriter().Write(frame); err != nil {
		return err
	}
	return control.Flush()
}

// finished returns the stream's capacity and lets a waiting Close proceed.
func (h *Hub) finished(s *Stream) {
	h.unsubscribe(s)
	h.stopped.Done()
}
