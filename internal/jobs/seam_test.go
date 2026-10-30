//go:build !benchslow

package jobs

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/store"
)

// seam is the untagged build's own slow-control hook. A test arms it, and the
// hook records that lane.go reached the seam before the claim published. It
// lives under !benchslow so the control build compiles benchslow.go's delay as
// the only implementation of the method.
var seam struct {
	mu    sync.Mutex
	armed bool
	ran   bool
}

// benchAttemptDelay is what an ordinary build satisfies the seam's assertion
// with: this declaration is untagged and test-only, so benchslow.go's control
// delay is the only other implementation of the method and no build compiles
// both. An ordinary build has no delay; this one only says the seam was reached.
func (*Runtime) benchAttemptDelay() {
	seam.mu.Lock()
	if seam.armed {
		seam.ran = true
	}
	seam.mu.Unlock()
}

// armSeam arms the hook for one test and reports whether the seam ran.
func armSeam(t *testing.T) func() bool {
	t.Helper()
	seam.mu.Lock()
	seam.armed, seam.ran = true, false
	seam.mu.Unlock()
	t.Cleanup(func() {
		seam.mu.Lock()
		seam.armed = false
		seam.mu.Unlock()
	})
	return func() bool {
		seam.mu.Lock()
		defer seam.mu.Unlock()
		return seam.ran
	}
}

// TestClaimPublishesTheCommittedStart proves the observable start AC-024's
// measurement samples: the claim a worker commits publishes exactly one running
// frame, after the slow-control seam and before the handler that runs the
// attempt, carrying the committed row's own revision, and the next transition's
// frame carries a strictly greater one.
func TestClaimPublishesTheCommittedStart(t *testing.T) {
	db := openTestStore(t)
	clock := newFakeClock()
	seamRan := armSeam(t)

	var (
		mu       sync.Mutex
		frames   []Event
		early    bool
		once     sync.Once
		observed = make(chan struct{})
		claimed  = make(chan int64, 1)
	)
	// The handler blocks until the start frame was published, so a publish that
	// landed after the handler fails on its own timeout rather than passing.
	work := handler("test.claim", func(ctx context.Context, a *Attempt) Outcome {
		select {
		case <-observed:
		case <-time.After(5 * time.Second):
			t.Error("the handler ran before the claim's start frame was published")
			return Outcome{Result: Failed, Reason: "unobserved"}
		}
		// The row this attempt holds, at the revision its claim committed.
		row, err := db.ReadJob(ctx, a.JobID)
		if err != nil {
			t.Errorf("ReadJob(%d): %v", a.JobID, err)
			return Outcome{Result: Failed, Reason: "read_failed"}
		}
		claimed <- row.Revision
		return Outcome{Result: Succeeded, Reason: "done"}
	})
	rt, err := New(db, Config{
		Now: clock.Now, After: clock.After, Boot: "boot", BackgroundSlots: 1,
		Log: func(string, ...any) {},
		Publish: func(ev Event) {
			mu.Lock()
			frames = append(frames, ev)
			if ev.State == store.JobRunning && !seamRan() {
				early = true
			}
			mu.Unlock()
			if ev.State == store.JobRunning {
				once.Do(func() { close(observed) })
			}
		},
	}, []Handler{work})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	if err := rt.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	id := enqueue(t, db, dueIntent(clock, "test.claim", "key"))
	stir(t, clock, "the attempt to finish", func() bool {
		row, err := db.ReadJob(t.Context(), id)
		return err == nil && row.State == store.JobSucceeded
	}, rt)
	var committed int64
	select {
	case committed = <-claimed:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never read the row it was claimed as")
	}

	mu.Lock()
	defer mu.Unlock()
	var starts, finishes []Event
	for _, ev := range frames {
		switch ev.State {
		case store.JobRunning:
			starts = append(starts, ev)
		case store.JobSucceeded:
			finishes = append(finishes, ev)
		}
	}
	if len(starts) != 1 {
		t.Fatalf("start frames = %d, want exactly one for one claim", len(starts))
	}
	start := starts[0]
	if start.JobID != id || start.Attempts != 1 {
		t.Fatalf("start frame = %+v, want job %d's first attempt", start, id)
	}
	if early {
		t.Fatal("the start frame was published before the slow-control seam")
	}
	if !seamRan() {
		t.Fatal("the slow-control seam never ran, so the publish's place was not observed")
	}
	if start.Revision != committed {
		t.Fatalf("the start frame states revision %d, want the committed row's %d", start.Revision, committed)
	}
	if len(finishes) != 1 {
		t.Fatalf("finish frames = %d, want the one transition that followed the claim", len(finishes))
	}
	if finishes[0].Revision <= start.Revision {
		t.Fatalf("the finish frame states revision %d, want it strictly greater than the start's %d", finishes[0].Revision, start.Revision)
	}
}
