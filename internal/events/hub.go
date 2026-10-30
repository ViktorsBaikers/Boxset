// Package events is the installation's one event stream: committed transitions
// are published once, kept in a bounded replay ring and delivered to authorized
// subscribers in sequence. Every frame is decided against current authority
// through auth.Revalidate, and a publisher never waits for a subscriber.
package events

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/auth"
)

// The recorded event bounds (architecture § Events contract).
const (
	// RingBytes and RingAge are the replay window's bounds.
	ringBytes = 1 << 20
	ringAge   = 5 * time.Minute
	// queueFrames and queueBytes bound one subscriber's undelivered frames.
	queueFrames = 256
	queueBytes  = 256 << 10
	// capPerCredential bounds one user's open streams (one credential's, for a
	// stream with no user); capGlobal bounds the installation's.
	capPerCredential = 4
	capGlobal        = 64
	// payloadBytes is the largest JSON payload one frame's data line may carry.
	payloadBytes = 8_192
	// frameDeadline bounds one frame's write and one frame's revalidation.
	frameDeadline = 10 * time.Second
	// heartbeatEvery is how often an idle stream proves its authority again.
	heartbeatEvery = 15 * time.Second
)

// The event names a committed transition publishes.
const (
	NameJob    = "job"
	NameUsers  = "users"
	NameSystem = "system"
)

// The refusal reasons the operation maps to a status.
const (
	ReasonStreamCap = "stream_cap"
	ReasonDraining  = "draining"
)

// Event is one committed transition: its name decides the right a subscriber
// needs, and its payload is encoded once, by Publish.
type Event struct {
	Name    string
	Payload any
}

// Error is one admission refusal: the recorded reason word and how long the
// caller should wait before reconnecting.
type Error struct {
	Reason     string
	RetryAfter time.Duration
}

func (e *Error) Error() string { return "events: " + e.Reason }

// EventHello is the first frame of every stream: the watermark a client
// snapshots at, and the identity it must confirm against.
type EventHello struct {
	Watermark    int64    `json:"watermark" minimum:"0"`
	InstallID    string   `json:"installId" pattern:"^[0-9a-f]{12}$"`
	RestoreEpoch int64    `json:"restoreEpoch" minimum:"1"`
	UserID       int64    `json:"userId" minimum:"0"`
	Credential   string   `json:"credential" enum:"cookie,key,local_bypass"`
	Rights       []string `json:"rights" maxItems:"3" uniqueItems:"true" nullable:"false" enum:"status:read,users:read,users:write"`
}

// EventResync tells a client its cursor cannot be honoured: it refetches its
// snapshot and opens a new stream from the recovery watermark.
type EventResync struct {
	Watermark int64  `json:"watermark" minimum:"0"`
	Reason    string `json:"reason" enum:"epoch,gap,invalid_id,overflow"`
}

// Config is what one hub is built from. Boot is the jobs runtime's start
// identity, so a frame id names the start it was produced by; InstallID and
// RestoreEpoch are the ready-time installation facts the hello frame carries;
// Revalidate is the one current-authority decision, and New refuses a hub
// without it.
type Config struct {
	Boot         string
	InstallID    string
	RestoreEpoch int64
	Now          func() time.Time
	Revalidate   func(context.Context, auth.CredentialRef) (auth.Principal, time.Time, error)
	Log          func(reason string, kv ...any)
	// Heartbeat overrides the recorded 15 s idle proof; a test may shorten it.
	Heartbeat time.Duration
}

// Hub is one installation's event stream. Publish never blocks on a subscriber:
// it encodes once under the hub mutex, keeps the frame in the bounded ring and
// hands it to each reachable subscriber's queue, which a subscriber drains on
// its own goroutine.
type Hub struct {
	cfg Config

	mu       sync.Mutex
	closed   bool
	seq      int64
	lower    int64 // no replay is possible at or below this seq
	ring     []ringEntry
	ringSize int
	streams  map[*Stream]struct{}
	byCap    map[string]int // open streams per cap key: a user, or a credential with none
	open     int

	stopped sync.WaitGroup
}

type ringEntry struct {
	seq   int64
	right string
	frame []byte
	at    time.Time
}

// New builds the hub. Revalidate is required: a hub without the one
// current-authority decision could only deliver stale grants, so the composition
// binds auth.Revalidate and a test binds the same function over a real store.
func New(cfg Config) *Hub {
	if cfg.Revalidate == nil {
		panic("events: Revalidate is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = heartbeatEvery
	}
	return &Hub{cfg: cfg, streams: map[*Stream]struct{}{}, byCap: map[string]int{}}
}

// Publish encodes one committed event and hands it to every subscriber whose
// rights reach it. It runs after the commit and never waits for a subscriber: an
// event too large to frame or one that cannot be encoded consumes its sequence,
// raises the replay lower bound and closes the affected streams with
// resync{overflow}, so nothing is stored, queued or written and nothing is
// silently skipped.
func (h *Hub) Publish(e Event) {
	right, ok := rightFor(e.Name)
	if !ok {
		h.log("event_refused", "reason", "event_unknown", "name", e.Name)
		return
	}
	payload, err := json.Marshal(e.Payload)
	// The decision and the fan-out are the only work under the mutex: the log
	// callback runs after it is released, so a slow log never delays a publisher.
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		h.log("event_dropped", "reason", ReasonDraining, "name", e.Name)
		return
	}
	h.seq++
	seq := h.seq
	if err != nil || len(payload) > payloadBytes {
		reason := "overflow"
		if err != nil {
			reason = "encode_failed"
		}
		h.lowerThrough(seq)
		for s := range h.streams {
			if s.reaches(right) {
				s.overflow(seq)
			}
		}
		h.mu.Unlock()
		// The encoded length is recorded before any bytes can be kept: the
		// payload itself is never stored, queued or written.
		h.log("event_unrepresentable", "reason", reason, "name", e.Name, "seq", seq, "encoded", len(payload))
		return
	}
	frame := encodeFrame(h.frameID(seq), e.Name, payload)
	h.append(ringEntry{seq: seq, right: right, frame: frame, at: h.cfg.Now()})
	for s := range h.streams {
		if s.reaches(right) {
			s.enqueue(seq, frame)
		}
	}
	h.mu.Unlock()
}

// PrincipalChanged closes every open stream of one principal's identity after a
// committed change to it, so a downgraded, disabled or signed-out viewer is not
// delivered another frame while it sits idle.
func (h *Hub) PrincipalChanged(userID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.streams {
		if s.user == userID {
			s.stop()
		}
	}
}

// drainGrace bounds how long Close waits for drained streams to deliver what
// they already queued. One frame's write is already bounded by frameDeadline, so
// a stream that has not ended after that is one whose reader stopped reading:
// Close stops it instead of waiting for it.
const drainGrace = frameDeadline

// Close ends every stream and refuses new ones: the installation is draining.
// Queued frames, the drain's own system frame included, are delivered first, and
// Close waits for that up to the recorded drain bound; a stream still open then is
// stopped, so a reader that stopped reading cannot hold the installation's stop.
func (h *Hub) Close() {
	h.mu.Lock()
	if !h.closed {
		h.closed = true
		for s := range h.streams {
			s.drain()
		}
	}
	h.mu.Unlock()
	if !h.await(drainGrace) {
		h.mu.Lock()
		for s := range h.streams {
			s.stop()
		}
		h.mu.Unlock()
	}
}

// await waits up to grace for the open streams to end and reports whether they
// all did. Admit takes a stream's count under the hub mutex before Close sets
// closed, so no stream joins the wait after it began.
func (h *Hub) await(grace time.Duration) bool {
	ended := make(chan struct{})
	go func() {
		h.stopped.Wait()
		close(ended)
	}()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-ended:
		return true
	case <-timer.C:
		return false
	}
}

// Admit reserves one stream and returns it ready to serve. The refusal is decided
// before the response head is written, because it is the route's status
// (429 stream_cap or 503 draining), not a frame. lastEventID is the client's
// Last-Event-ID header, empty on a first connection.
func (h *Hub) Admit(principal auth.Principal, ref auth.CredentialRef, lastEventID string) (*Stream, error) {
	if !ref.Valid() {
		return nil, &Error{Reason: "credential_invalid"}
	}
	key := capKey(ref)
	h.mu.Lock()
	defer h.mu.Unlock()
	if refusal := h.refusalLocked(key); refusal != nil {
		return nil, refusal
	}
	h.byCap[key]++
	h.open++
	h.stopped.Add(1)
	s := &Stream{
		hub:       h,
		principal: principal,
		ref:       ref,
		cap:       key,
		user:      ref.User(),
		watermark: h.seq,
		notify:    make(chan struct{}, 1),
	}
	// Subscribe and read the sequence under one mutex, so the watermark, the
	// replay window and the live queue cannot disagree by one event.
	if lastEventID != "" {
		if reason := h.replay(s, lastEventID); reason != "" {
			s.resync = reason
		}
	}
	h.streams[s] = struct{}{}
	return s, nil
}

// Refusal is the answer a new stream of ref would get right now, without
// reserving one: a HEAD of the stream route advertises the status its GET would
// get, so it cannot promise a stream the hub would refuse while it holds no slot.
func (h *Hub) Refusal(ref auth.CredentialRef) *Error {
	if !ref.Valid() {
		return &Error{Reason: "credential_invalid"}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.refusalLocked(capKey(ref))
}

// capKey names the stream cap's counter: a stream that names a user is counted by
// that user, so one account's sessions and keys share one cap and cannot take
// every installation slot; a stream with no user (the direct-peer grant) is
// counted by its credential.
func capKey(ref auth.CredentialRef) string {
	if user := ref.User(); user != 0 {
		return "user:" + strconv.FormatInt(user, 10)
	}
	return ref.Identity()
}

// refusalLocked is the admission decision Admit and Refusal share, so a HEAD and
// the GET it aliases cannot disagree. It runs under the hub mutex.
func (h *Hub) refusalLocked(key string) *Error {
	if h.closed {
		return &Error{Reason: ReasonDraining}
	}
	if h.byCap[key] >= capPerCredential || h.open >= capGlobal {
		return &Error{Reason: ReasonStreamCap, RetryAfter: 5 * time.Second}
	}
	return nil
}

// replay queues the frames a reconnecting client missed, or names the reason its
// cursor cannot be honoured.
func (h *Hub) replay(s *Stream, lastEventID string) string {
	// Entries older than the window are gone even if nothing was appended since:
	// the bounds are read now, not only when a frame arrives.
	h.trimRing()
	epoch, boot, seq, ok := parseFrameID(lastEventID)
	if !ok {
		return "invalid_id"
	}
	if epoch != h.cfg.RestoreEpoch || boot != h.cfg.Boot {
		return "epoch"
	}
	if seq < 0 || seq > h.seq {
		return "invalid_id"
	}
	if seq == h.seq {
		return ""
	}
	if seq < h.lower || (len(h.ring) > 0 && seq+1 < h.ring[0].seq) || (len(h.ring) == 0 && seq < h.seq) {
		return "gap"
	}
	for _, entry := range h.ring {
		if entry.seq <= seq || !s.reaches(entry.right) {
			continue
		}
		s.replay = append(s.replay, entry.frame)
	}
	return ""
}

// unsubscribe returns a finished stream's capacity.
func (h *Hub) unsubscribe(s *Stream) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.streams[s]; !ok {
		return
	}
	delete(h.streams, s)
	h.open--
	h.byCap[s.cap]--
	if h.byCap[s.cap] <= 0 {
		delete(h.byCap, s.cap)
	}
}

// frameID is one frame's id: the restore epoch, the boot and the sequence.
func (h *Hub) frameID(seq int64) string {
	return strconv.FormatInt(h.cfg.RestoreEpoch, 10) + "." + h.cfg.Boot + "." + strconv.FormatInt(seq, 10)
}

// parseFrameID splits the three recorded parts of a frame id. It cuts twice
// instead of splitting, so a cursor of nothing but separators allocates no slice
// per part while it runs under the publish mutex.
func parseFrameID(id string) (epoch int64, boot string, seq int64, ok bool) {
	epochPart, rest, found := strings.Cut(id, ".")
	if !found {
		return 0, "", 0, false
	}
	boot, seqPart, found := strings.Cut(rest, ".")
	if !found || strings.Contains(seqPart, ".") {
		return 0, "", 0, false
	}
	epoch, err := strconv.ParseInt(epochPart, 10, 64)
	if err != nil {
		return 0, "", 0, false
	}
	seq, err = strconv.ParseInt(seqPart, 10, 64)
	if err != nil {
		return 0, "", 0, false
	}
	return epoch, boot, seq, true
}

// rightFor is the right one event name's frames are delivered under. The system
// state reaches every authorized reader; a job needs the status right and a user
// change the users right.
func rightFor(name string) (string, bool) {
	switch name {
	case NameJob:
		return auth.RightStatusRead, true
	case NameUsers:
		return auth.RightUsersRead, true
	case NameSystem:
		return "", true
	}
	return "", false
}

// append keeps one frame in the bounded ring, dropping what aged out first.
func (h *Hub) append(entry ringEntry) {
	h.ring = append(h.ring, entry)
	h.ringSize += len(entry.frame)
	h.trimRing()
}

func (h *Hub) trimRing() {
	now := h.cfg.Now()
	drop := 0
	for drop < len(h.ring) && (now.Sub(h.ring[drop].at) >= ringAge || h.ringSize > ringBytes) {
		h.ringSize -= len(h.ring[drop].frame)
		drop++
	}
	if drop == 0 {
		return
	}
	h.ring = append(h.ring[:0], h.ring[drop:]...)
}

// lowerThrough drops every replay entry at or below seq: the client's only
// recovery from there is a snapshot and resync.
func (h *Hub) lowerThrough(seq int64) {
	if seq > h.lower {
		h.lower = seq
	}
	drop := 0
	for drop < len(h.ring) && h.ring[drop].seq <= seq {
		h.ringSize -= len(h.ring[drop].frame)
		drop++
	}
	if drop > 0 {
		h.ring = append(h.ring[:0], h.ring[drop:]...)
	}
}

// encodeFrame is one server-sent frame: at most one id, one event and one data
// line, then the blank line that ends it.
func encodeFrame(id, name string, payload []byte) []byte {
	frame := make([]byte, 0, len(id)+len(name)+len(payload)+32)
	frame = append(frame, "id: "...)
	frame = append(frame, id...)
	frame = append(frame, '\n', 'e', 'v', 'e', 'n', 't', ':', ' ')
	frame = append(frame, name...)
	frame = append(frame, '\n', 'd', 'a', 't', 'a', ':', ' ')
	frame = append(frame, payload...)
	frame = append(frame, '\n', '\n')
	return frame
}

func (h *Hub) log(reason string, kv ...any) {
	if h.cfg.Log != nil {
		h.cfg.Log(reason, kv...)
	}
}
