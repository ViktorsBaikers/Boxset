//go:build integration && unix
// +build integration,unix

package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/outbound"
	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// The reconciliation contract of one accepted mutation, proved against a real
// loopback service: a write the peer applied without answering is decided by the
// peer's own identity list, and nothing is resent before that decision.
const (
	remoteKind     = "remote.mutation"
	remoteIdentity = "synthetic-item-1"
	// remoteChildDir is how this binary re-executes itself as the process that dies
	// after the peer applied its write.
	remoteChildDir = "BOXSET_REMOTE_CHILD_DIR"
)

// remoteService is the peer: a loopback client-shaped service that owns the
// identities it accepted in a file, so the write outlives the process that sent it.
type remoteService struct {
	dir    string
	srv    *httptest.Server
	origin outbound.Origin

	adds     atomic.Int64
	lossy    atomic.Bool
	lookupOK atomic.Bool
	kill     atomic.Bool
	mu       sync.Mutex
}

// newRemoteService starts the peer over dir's identity file.
func newRemoteService(t *testing.T, dir string) *remoteService {
	t.Helper()
	service := &remoteService{dir: dir}
	service.lookupOK.Store(true)
	service.srv = httptest.NewServer(http.HandlerFunc(service.serve))
	t.Cleanup(service.srv.Close)
	parsed, err := netip.ParseAddrPort(strings.TrimPrefix(service.srv.URL, "http://"))
	if err != nil {
		t.Fatalf("peer address in %q: %v", service.srv.URL, err)
	}
	service.origin = outbound.Origin{Scheme: "http", Host: parsed.Addr().Unmap().String(), Port: int(parsed.Port())}
	return service
}

// serve is the peer's whole surface: one write, and the page walk the client reads
// back to decide what happened.
func (s *remoteService) serve(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if !s.lookupOK.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		page := struct {
			Identities []string `json:"identities"`
			Next       string   `json:"next"`
		}{Identities: s.identities()}
		_ = json.NewEncoder(w).Encode(page)
	case http.MethodPost:
		s.adds.Add(1)
		body := make([]byte, 1024)
		read, _ := r.Body.Read(body)
		s.record(strings.TrimSpace(string(body[:read])))
		if s.kill.Load() {
			// The peer applied the write and this process died before it could
			// answer: the caller's next owner has to look the effect up.
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			return
		}
		if s.lossy.CompareAndSwap(true, false) {
			// The write is applied and the acknowledgement is lost.
			if hijacker, ok := w.(http.Hijacker); ok {
				if connection, _, err := hijacker.Hijack(); err == nil {
					_ = connection.Close()
					return
				}
			}
		}
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// record keeps one applied identity on disk.
func (s *remoteService) record(identity string) {
	if identity == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	file, err := os.OpenFile(filepath.Join(s.dir, "identities"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()
	_, _ = fmt.Fprintln(file, identity)
}

// identities is what the peer applied, read back from its own record.
func (s *remoteService) identities() []string {
	raw, err := os.ReadFile(filepath.Join(s.dir, "identities"))
	if err != nil {
		return nil
	}
	var held []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		if line != "" {
			held = append(held, line)
		}
	}
	return held
}

// op is the mutation one attempt sends.
func (s *remoteService) op(identity string) outbound.Op {
	target := &outbound.Target{ID: "remote", Origin: s.origin, Private: true}
	return outbound.Op{
		Name:   remoteKind,
		Target: target,
		Method: http.MethodPost,
		URL:    &url.URL{Scheme: "http", Host: net.JoinHostPort(s.origin.Host, strconv.Itoa(s.origin.Port)), Path: "/items"},
		Body:   identity,
	}
}

// fetch reads one page of the peer's identities through the client's own safe read.
func (s *remoteService) fetch(client *outbound.Client) func(context.Context, string) (outbound.Page, error) {
	return func(ctx context.Context, cursor string) (outbound.Page, error) {
		target := &outbound.Target{ID: "remote", Origin: s.origin, Private: true}
		query := "cursor=" + cursor
		response, err := client.Do(ctx, outbound.Op{
			Name:   remoteKind + ".locate",
			Target: target,
			Method: http.MethodGet,
			URL:    &url.URL{Scheme: "http", Host: net.JoinHostPort(s.origin.Host, strconv.Itoa(s.origin.Port)), Path: "/items", RawQuery: query},
			Safe:   true,
		}, nil)
		if err != nil {
			return outbound.Page{}, err
		}
		var page struct {
			Identities []string `json:"identities"`
			Next       string   `json:"next"`
		}
		if err := json.Unmarshal([]byte(response.Body), &page); err != nil {
			return outbound.Page{}, err
		}
		return outbound.Page{Identities: page.Identities, Next: page.Next}, nil
	}
}

// handler is the kind one accepted mutation runs as: the caller's own slot is the
// one a mutation sends in, and a wait goes through Attempt.Wait so the slot is free
// while nothing is sent.
func (s *remoteService) handler(client *outbound.Client) Handler {
	return Handler{
		Kind:        remoteKind,
		Class:       ClassRequired,
		Lane:        LaneBackground,
		MaxAttempts: 3,
		Deadline:    10 * time.Second,
		Run: func(ctx context.Context, attempt *Attempt) Outcome {
			_, err := client.Do(ctx, s.op(attempt.Params["identity"]), attempt.Wait)
			var refused *outbound.Error
			switch {
			case err == nil:
				return Outcome{Result: Succeeded, Reason: "applied"}
			case errors.As(err, &refused) && refused.Unknown:
				return Outcome{Result: Unknown, Reason: refused.Reason}
			case errors.As(err, &refused) && refused.Reason == outbound.ReasonNotSent:
				return Outcome{Result: Retry, Reason: refused.Reason, RetryAfter: 10 * time.Millisecond}
			}
			return Outcome{Result: Failed, Reason: "mutation_failed"}
		},
		Reconcile: func(ctx context.Context, attempt *Attempt) Outcome {
			presence, err := outbound.Locate(ctx, s.fetch(client), attempt.Params["identity"], 8)
			if err != nil {
				return Outcome{Result: Failed, Reason: "lookup_unavailable"}
			}
			switch presence {
			case outbound.PresencePresent:
				return Outcome{Result: Succeeded, Reason: "reconciled_present"}
			case outbound.PresenceAbsent:
				return Outcome{Result: Retry, Reason: "reconciled_absent", RetryAfter: 10 * time.Millisecond}
			}
			return Outcome{Result: Failed, Reason: "presence_ambiguous"}
		},
	}
}

// remoteRuntime is one started runtime over dir's store, with a clock whose waits
// never fire on their own: the proof decides when a sweep happens.
func remoteRuntime(t *testing.T, dir string, service *remoteService) (*Runtime, *store.DB, *outbound.Client) {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(dir, "boxset.db"), store.Options{})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	client := outbound.New(outbound.Config{
		Resolver: fixedResolver{"127.0.0.1": {netip.MustParseAddr("127.0.0.1")}},
		Slot:     func(context.Context) (func(), error) { return func() {}, nil },
		Version:  "test",
	})
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	// No Boot: every start mints its own lease identity, which is what makes the
	// rows of a process that died recognisable as foreign.
	runtime, err := New(db, Config{
		BackgroundSlots: 1,
		Log:             func(string, ...any) {},
		After:           func(time.Duration) <-chan time.Time { return make(chan time.Time) },
	}, []Handler{service.handler(client)})
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	return runtime, db, client
}

// fixedResolver answers the loopback name the peer is reached by.
type fixedResolver map[string][]netip.Addr

func (r fixedResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	if network != "ip" {
		return nil, &net.DNSError{Err: "unsupported network", Name: host}
	}
	answer, ok := r[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return answer, nil
}

// accept enqueues the one accepted mutation.
func accept(t *testing.T, db *store.DB, identity string) int64 {
	t.Helper()
	var id int64
	if err := db.Write(t.Context(), func(tx *sql.Tx) error {
		created, _, err := store.EnqueueJob(t.Context(), tx, store.JobIntent{
			Kind:           remoteKind,
			Class:          store.JobRequired,
			Lane:           store.JobBackground,
			DedupeKey:      identity,
			ConcurrencyKey: identity,
			Params:         map[string]string{"identity": identity},
			MaxAttempts:    3,
			DueAt:          time.Now().Unix(),
		})
		id = created
		return err
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return id
}

// awaitState waits until the row reaches one of the states.
func awaitState(t *testing.T, db *store.DB, id int64, states ...string) store.Job {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		row, err := db.ReadJob(t.Context(), id)
		if err != nil {
			t.Fatalf("ReadJob: %v", err)
		}
		for _, state := range states {
			if row.State == state {
				return row
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d stayed %q, want one of %v", id, row.State, states)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestLostAck proves the accepted write the peer applied without an answer is
// decided by its own identity list: one add, no resend, and the row succeeds.
func TestLostAck(t *testing.T) {
	dir := t.TempDir()
	service := newRemoteService(t, dir)
	service.lossy.Store(true)
	runtime, db, _ := remoteRuntime(t, dir, service)
	id := accept(t, db, remoteIdentity)
	if err := runtime.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = runtime.Stop(stopCtx)
	}()

	awaitState(t, db, id, store.JobReconciling)
	if err := runtime.sweepOnce(t.Context()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	row := awaitState(t, db, id, store.JobSucceeded, store.JobReview, store.JobFailed)
	if row.State != store.JobSucceeded || row.ReasonCode != "reconciled_present" {
		t.Fatalf("the reconciled row = %+v, want the peer's own identity list to have decided it applied", row)
	}
	if got := service.adds.Load(); got != 1 {
		t.Fatalf("the peer received %d writes, want the one it lost the acknowledgement for", got)
	}
	if held := service.identities(); len(held) != 1 {
		t.Fatalf("the peer holds %d identities, want exactly one", len(held))
	}
}

// TestUncertainAbsence proves an unprovable lookup keeps the row visible in review
// with its key held: the write is never repeated, not even across a restart.
func TestUncertainAbsence(t *testing.T) {
	dir := t.TempDir()
	service := newRemoteService(t, dir)
	service.lossy.Store(true)
	runtime, db, _ := remoteRuntime(t, dir, service)
	id := accept(t, db, remoteIdentity)
	if err := runtime.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitState(t, db, id, store.JobReconciling)
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := runtime.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// The next start cannot read the peer's list: the row stays paused in review.
	service.lookupOK.Store(false)
	restarted, _, _ := remoteRuntime(t, dir, service)
	if err := restarted.Start(t.Context()); err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = restarted.Stop(stopCtx)
	}()
	row := awaitState(t, db, id, store.JobReview, store.JobSucceeded, store.JobFailed)
	if row.State != store.JobReview || row.ReasonCode != "lookup_unavailable" {
		t.Fatalf("the row after the restart = %+v, want review with the unprovable lookup", row)
	}
	if got := service.adds.Load(); got != 1 {
		t.Fatalf("the peer received %d writes, want no resend across the restart", got)
	}
}

// TestKilledMutation proves a process that dies after the peer applied its write
// resends nothing: the next start looks the effect up before any new attempt.
func TestKilledMutation(t *testing.T) {
	dir := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=TestKilledMutationChild", "-test.timeout=120s")
	command.Env = append(os.Environ(), remoteChildDir+"="+dir)
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("the child that writes and dies: %v\n%s", err, output)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || status.Signal() != syscall.SIGKILL {
		t.Fatalf("the child exited %v, want the kill after its write\n%s", err, output)
	}

	service := newRemoteService(t, dir)
	if held := service.identities(); len(held) != 1 || held[0] != remoteIdentity {
		t.Fatalf("the peer holds %v, want the one identity the dead process wrote", held)
	}
	runtime, db, _ := remoteRuntime(t, dir, service)
	if err := runtime.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = runtime.Stop(stopCtx)
	}()
	rows, err := db.ReconcilingJobs(t.Context())
	if err != nil {
		t.Fatalf("ReconcilingJobs: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("%d rows are still reconciling after the start, want every one decided", len(rows))
	}
	var state, reason string
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT state, COALESCE(reason_code, '') FROM job WHERE kind = ?`, remoteKind).Scan(&state, &reason)
	}); err != nil {
		t.Fatalf("the accepted row: %v", err)
	}
	if state != store.JobSucceeded || reason != "reconciled_present" {
		t.Fatalf("the row after the kill = %s/%s, want the lookup to have decided it applied", state, reason)
	}
	if got := service.adds.Load(); got != 0 {
		t.Fatalf("the peer received %d writes from the new process, want none before the lookup", got)
	}
}

// TestKilledMutationChild is the process that writes and dies: it sends one
// accepted mutation to a peer that applies it and kills this process before it can
// answer.
func TestKilledMutationChild(t *testing.T) {
	dir := os.Getenv(remoteChildDir)
	if dir == "" {
		t.Skip("not the child process")
	}
	service := newRemoteService(t, dir)
	service.kill.Store(true)
	runtime, db, _ := remoteRuntime(t, dir, service)
	accept(t, db, remoteIdentity)
	if err := runtime.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-time.After(60 * time.Second):
		t.Fatal("the write never reached the peer")
	}
}
