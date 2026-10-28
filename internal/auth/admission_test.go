package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/store"
)

func TestHashWaiterPopulation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := NewHasher()
		h.slots <- struct{}{}
		h.slots <- struct{}{}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var wg sync.WaitGroup
		for range 32 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := h.acquire(ctx); !errors.Is(err, context.Canceled) {
					t.Errorf("waiter=%v", err)
				}
			}()
		}
		synctest.Wait()
		validPHC := phcPrefix + "AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		if _, err := h.Hash(t.Context(), "correct horse battery staple"); !errors.Is(err, ErrHashBusy) {
			t.Errorf("hash overflow=%v", err)
		}
		if _, err := h.Verify(t.Context(), validPHC, "correct horse battery staple"); !errors.Is(err, ErrHashBusy) {
			t.Errorf("verify overflow=%v", err)
		}
		if _, err := h.Verify(t.Context(), "$argon2id$v=19$m=1,t=1,p=1$foreign$foreign", "password"); !errors.Is(err, ErrPHCInvalid) {
			t.Errorf("foreign PHC=%v", err)
		}
		overflow := make(chan error, 1)
		go func() { overflow <- h.acquire(ctx) }()
		synctest.Wait()
		select {
		case err := <-overflow:
			if !errors.Is(err, ErrHashBusy) {
				t.Errorf("overflow=%v", err)
			}
		default:
			t.Error("HASH-WAITER-CAP: the 33rd waiter queued instead of immediate hash_busy")
		}
		cancel()
		wg.Wait()
		synctest.Wait()
		if len(h.waiters) != 0 {
			t.Fatalf("leaked hash waiter population=%d", len(h.waiters))
		}
		<-h.slots
		<-h.slots
		for range 64 {
			if err := h.acquire(ctx); !errors.Is(err, context.Canceled) {
				t.Errorf("canceled free slot admitted: %v", err)
				if err == nil {
					h.release()
				}
			}
		}
		h.wait = time.Second
		if err := h.acquire(t.Context()); err != nil {
			t.Fatal(err)
		}
		h.release()
		for range 2 {
			h.slots <- struct{}{}
		}
		promoted := make(chan error, 1)
		go func() { promoted <- h.acquire(t.Context()) }()
		synctest.Wait()
		if len(h.waiters) != 1 {
			t.Fatal("promotion cohort was not waiting")
		}
		h.release()
		if err := <-promoted; err != nil {
			t.Fatal(err)
		}
		if len(h.waiters) != 0 || len(h.slots) != 2 {
			t.Fatal("promotion retained a waiter or lost a live hash slot")
		}
		h.release()
		h.release()
		for range 2 {
			h.slots <- struct{}{}
		}
		expired := make(chan error, 1)
		go func() { expired <- h.acquire(t.Context()) }()
		synctest.Wait()
		time.Sleep(h.wait)
		if err := <-expired; !errors.Is(err, ErrHashBusy) {
			t.Fatalf("expired waiter=%v", err)
		}
		if len(h.waiters) != 0 || len(h.slots) != 2 {
			t.Fatal("timeout retained waiter or released another caller's active slot")
		}
		h.release()
		h.release()
	})
}

func TestPasswordReadHeadroom(t *testing.T) {
	db := openStore(t, t.TempDir())
	t.Cleanup(func() { _ = db.Close() })
	synctest.Test(t, func(t *testing.T) {
		h := NewHasher()
		s := NewSetup(db, h, t.TempDir())
		h.readTurn <- struct{}{}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 32)
		for i := range 32 {
			go func() {
				var err error
				switch i % 3 {
				case 0:
					_, err = s.StateForPassword(ctx)
				case 1:
					_, err = s.Create(ctx, [32]byte{1}, "owner", "correct horse battery staple")
				case 2:
					_, _, _, err = s.Sessions().Login(ctx, "owner", "correct horse battery staple")
				}
				done <- err
			}()
		}
		synctest.Wait()
		// Only the winner of Create's separate turn owns a database-stage permit.
		if len(h.passwords) != 22 {
			t.Fatalf("password population=%d want22 (21 state/login + one Create)", len(h.passwords))
		}
		for len(h.passwords) < 32 {
			h.passwords <- struct{}{}
		}
		if _, err := s.StateForPassword(t.Context()); !errors.Is(err, ErrHashBusy) {
			t.Errorf("state overflow=%v", err)
		}
		if _, _, _, err := s.Sessions().Login(t.Context(), "owner", "correct horse battery staple"); !errors.Is(err, ErrHashBusy) {
			t.Errorf("login overflow=%v", err)
		}
		cancel()
		for range 32 {
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Errorf("canceled seam=%v", err)
			}
		}
		for len(h.passwords) > 0 {
			<-h.passwords
		}
		<-h.readTurn
		if len(s.turn) != 0 || len(h.slots) != 0 || len(h.waiters) != 0 {
			t.Fatal("password stage retained a creation/hash permit")
		}
		started := time.Now()
		h.readTurn <- struct{}{}
		deadline := make(chan time.Time, 1)
		result := make(chan error, 1)
		go func() {
			result <- h.readPassword(t.Context(), db, func(ctx context.Context, tx *sql.Tx) error {
				at, _ := ctx.Deadline()
				deadline <- at
				var n int
				return tx.QueryRowContext(ctx, "SELECT 42").Scan(&n)
			})
		}()
		synctest.Wait()
		time.Sleep(time.Second)
		<-h.readTurn
		if at := <-deadline; !at.Equal(started.Add(2 * time.Second)) {
			t.Fatalf("password deadline reset after turn wait: %v want %v", at, started.Add(2*time.Second))
		}
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		if len(h.passwords) != 0 || len(h.readTurn) != 0 {
			t.Fatal("completed SQL stage leaked population or turn")
		}
	})
	for _, capacity := range []int{1, 2, 4} {
		t.Run(fmt.Sprintf("readers_%d", capacity), func(t *testing.T) { passwordReadCapacity(t, capacity, false) })
		t.Run(fmt.Sprintf("deadline_%d", capacity), func(t *testing.T) { passwordReadDeadline(t, capacity) })
		t.Run(fmt.Sprintf("hash_write_%d", capacity), func(t *testing.T) { passwordReadHashWrite(t, capacity) })
	}
	t.Run("shrink_4_to_2", func(t *testing.T) { passwordReadCapacity(t, 4, true) })
}

type passwordReadAcknowledgement struct {
	context.Context
	entered chan<- struct{}
	once    sync.Once
}

func (c *passwordReadAcknowledgement) Deadline() (time.Time, bool) {
	c.once.Do(func() { c.entered <- struct{}{} })
	return c.Context.Deadline()
}

func passwordCapacitySetup(t *testing.T, capacity int) (*store.DB, *Setup) {
	t.Helper()
	previous := runtime.GOMAXPROCS(capacity)
	t.Cleanup(func() { runtime.GOMAXPROCS(previous) })
	dir := t.TempDir()
	db := openStore(t, dir)
	t.Cleanup(func() { _ = db.Close() })
	s := NewSetup(db, NewHasher(), dir)
	if _, err := s.Arm(t.Context(), "synthetic-capacity-setup"); err != nil {
		t.Fatal(err)
	}
	return db, s
}

func passwordCapacityQuery(ctx context.Context, tx *sql.Tx) error {
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT 42").Scan(&n); err != nil {
		return err
	}
	if n != 42 {
		return errors.New("ordinary query returned the wrong value")
	}
	return nil
}

func passwordReadCapacity(t *testing.T, capacity int, shrink bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		db, s := passwordCapacitySetup(t, capacity)
		const password = "correct horse battery staple"
		v := Verifier("synthetic-capacity-setup")
		if _, err := s.Create(t.Context(), v, "owner", password); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		cohorts := 1
		if capacity == 1 {
			cohorts = 3
		}
		for cohort := range cohorts {
			held, release, passwordDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			go func() {
				passwordDone <- s.hasher.readPassword(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
					if err := passwordCapacityQuery(ctx, tx); err != nil {
						return err
					}
					close(held)
					<-release
					return nil
				})
			}()
			<-held
			var releaseOnce sync.Once
			finishPassword := func() { releaseOnce.Do(func() { close(release) }) }
			defer finishPassword()
			var oldOrdinary []chan struct{}
			oldDone := make(chan error, 3)
			if shrink {
				for range 3 {
					entered, gate := make(chan struct{}), make(chan struct{})
					oldOrdinary = append(oldOrdinary, gate)
					defer func() {
						select {
						case <-gate:
						default:
							close(gate)
						}
					}()
					go func() {
						oldDone <- db.Read(ctx, func(tx *sql.Tx) error {
							if err := passwordCapacityQuery(ctx, tx); err != nil {
								return err
							}
							close(entered)
							<-gate
							return nil
						})
					}()
					<-entered
				}
				if stats := db.Stats().Reader; stats.InUse != 4 || stats.MaxOpenConnections != 4 {
					t.Fatalf("pre-shrink readers=%+v", stats)
				}
				runtime.GOMAXPROCS(2)
			}
			admitted, done := make(chan struct{}, 31), make(chan error, 31)
			for i := range 31 {
				go func() {
					call := &passwordReadAcknowledgement{Context: ctx, entered: admitted}
					var err error
					switch i {
					case 0:
						_, err = s.Create(call, v, "owner", password)
						if errors.Is(err, ErrSetupCompleted) {
							err = nil
						} else if err == nil {
							err = errors.New("consumed setup created another user")
						}
					case 1:
						var session Session
						var token string
						session, token, _, err = s.Sessions().Login(call, "owner", password)
						if err == nil && (session.User.ID != 1 || session.User.Role != RoleAdmin || len(token) != 43) {
							err = errors.New("valid login returned the wrong identity")
						}
					default:
						// The setup and login guards call this same shared service.
						var state SetupState
						state, err = s.StateForPassword(call)
						if err == nil && (state.Open || !state.Matches(v)) {
							err = fmt.Errorf("guard role %d read invalid setup state", i%2)
						}
					}
					done <- err
				}()
			}
			synctest.Wait()
			for range 31 {
				select {
				case <-admitted:
				default:
					t.Fatal("one of the four real auth seams bypassed shared stage admission")
				}
			}
			select {
			case err := <-done:
				t.Fatalf("auth work completed while the password transaction was held: %v", err)
			default:
			}
			before := time.Now()
			if _, err := s.StateForPassword(ctx); !errors.Is(err, ErrHashBusy) {
				t.Fatalf("shared state overflow=%v", err)
			}
			if _, _, _, err := s.Sessions().Login(ctx, "owner", password); !errors.Is(err, ErrHashBusy) {
				t.Fatalf("shared login overflow=%v", err)
			}
			if !time.Now().Equal(before) {
				t.Fatal("population overflow waited instead of refusing immediately")
			}
			if !shrink && db.Stats().Reader.InUse != 1 {
				t.Fatal("more than one password transaction entered the physical pool")
			}
			ordinaryEntered, ordinaryRelease, ordinaryDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			var ordinaryOnce sync.Once
			finishOrdinary := func() { ordinaryOnce.Do(func() { close(ordinaryRelease) }) }
			defer finishOrdinary()
			go func() {
				ordinaryDone <- db.Read(ctx, func(tx *sql.Tx) error {
					if err := passwordCapacityQuery(ctx, tx); err != nil {
						return err
					}
					close(ordinaryEntered)
					<-ordinaryRelease
					return nil
				})
			}()
			synctest.Wait()
			if shrink {
				if stats := db.Stats().Reader; stats.MaxOpenConnections != 2 || stats.InUse != 4 {
					t.Fatalf("shrink admitted new work before old readers drained: %+v", stats)
				}
				for _, gate := range oldOrdinary {
					close(gate)
				}
				for range 3 {
					if err := <-oldDone; err != nil {
						t.Fatal(err)
					}
				}
				synctest.Wait()
			}
			if capacity == 1 {
				select {
				case <-ordinaryEntered:
					t.Fatal("one reader falsely offered simultaneous headroom")
				default:
				}
				finishPassword()
				if err := <-passwordDone; err != nil {
					t.Fatal(err)
				}
			}
			<-ordinaryEntered
			select {
			case err := <-done:
				t.Fatalf("ordinary turn was stolen by queued auth work: %v", err)
			default:
			}
			want, maximum := 2, capacity
			if capacity == 1 {
				want = 1
			}
			if shrink {
				maximum = 2
			}
			if stats := db.Stats().Reader; stats.InUse != want || stats.MaxOpenConnections != maximum {
				t.Fatalf("held reader population=%+v want in-use %d maximum %d", stats, want, maximum)
			}
			if capacity == 1 {
				finishOrdinary()
				if err := <-ordinaryDone; err != nil {
					t.Fatal(err)
				}
			}
			if capacity != 1 {
				finishPassword()
				if err := <-passwordDone; err != nil {
					t.Fatal(err)
				}
			}
			for range 31 {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			if capacity != 1 {
				finishOrdinary()
				if err := <-ordinaryDone; err != nil {
					t.Fatal(err)
				}
			}
			if len(s.hasher.passwords) != 0 || len(s.hasher.readTurn) != 0 || len(s.turn) != 0 {
				t.Fatal("auth cohort retained admission state")
			}
			if err := db.Read(ctx, func(tx *sql.Tx) error {
				var users, sessions int
				if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM user").Scan(&users); err != nil {
					return err
				}
				if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM session").Scan(&sessions); err != nil {
					return err
				}
				if users != 1 || sessions != cohort+1 {
					return errors.New("auth cohort did not preserve atomic user/session state")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func passwordReadDeadline(t *testing.T, capacity int) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		db, s := passwordCapacitySetup(t, capacity)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		held, release, holder := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		go func() {
			holder <- db.ReadPassword(ctx, func(tx *sql.Tx) error {
				if err := passwordCapacityQuery(ctx, tx); err != nil {
					return err
				}
				close(held)
				<-release
				return nil
			})
		}()
		<-held
		var once sync.Once
		finish := func() { once.Do(func() { close(release) }) }
		defer finish()
		s.hasher.readTurn <- struct{}{}
		admitted, done := make(chan struct{}, 4), make(chan error, 4)
		for i := range 4 {
			go func() {
				call := &passwordReadAcknowledgement{Context: ctx, entered: admitted}
				var err error
				switch i {
				case 0, 1:
					_, err = s.StateForPassword(call)
				case 2:
					_, err = s.Create(call, Verifier("synthetic-capacity-setup"), "owner", "correct horse battery staple")
				case 3:
					_, _, _, err = s.Sessions().Login(call, "owner", "correct horse battery staple")
				}
				done <- err
			}()
		}
		synctest.Wait()
		for range 4 {
			select {
			case <-admitted:
			default:
				t.Fatal("deadline cohort did not reach each auth seam")
			}
		}
		time.Sleep(time.Second)
		<-s.hasher.readTurn
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		for range 4 {
			select {
			case err := <-done:
				if !errors.Is(err, ErrHashBusy) {
					t.Fatalf("shared two-second deadline=%v", err)
				}
			default:
				t.Fatal("auth/store admission reset the original two-second deadline")
			}
		}
		if len(s.hasher.passwords) != 0 || len(s.hasher.readTurn) != 0 || len(s.turn) != 0 {
			t.Fatal("deadline retained admission state")
		}
		finish()
		if err := <-holder; err != nil {
			t.Fatal(err)
		}
	})
}

func passwordReadHashWrite(t *testing.T, capacity int) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		db, s := passwordCapacitySetup(t, capacity)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		entered, release, writer := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		go func() {
			writer <- db.Write(ctx, func(tx *sql.Tx) error {
				if err := passwordCapacityQuery(ctx, tx); err != nil {
					return err
				}
				close(entered)
				<-release
				return nil
			})
		}()
		<-entered
		var once sync.Once
		finishWriter := func() { once.Do(func() { close(release) }) }
		defer finishWriter()
		s.hasher.slots <- struct{}{}
		s.hasher.slots <- struct{}{}
		done := make(chan error, 1)
		go func() {
			user, err := s.Create(ctx, Verifier("synthetic-capacity-setup"), "owner", "correct horse battery staple")
			if err == nil && (user.ID != 1 || user.Role != RoleAdmin) {
				err = errors.New("setup returned invalid admin")
			}
			done <- err
		}()
		synctest.Wait()
		if len(s.hasher.waiters) != 1 {
			t.Fatal("Create did not finish its state read before waiting for hash admission")
		}
		check := func() {
			if len(s.hasher.passwords) != 0 || len(s.hasher.readTurn) != 0 {
				t.Fatal("Create retained its password-read stage during hash/write wait")
			}
			for range 2 {
				state, err := s.StateForPassword(ctx)
				if err != nil || !state.Open || !state.Matches(Verifier("synthetic-capacity-setup")) {
					t.Fatalf("guard progress during hash/write wait: open=%v error=%v", state.Open, err)
				}
			}
			if err := db.Read(ctx, func(tx *sql.Tx) error {
				if err := passwordCapacityQuery(ctx, tx); err != nil {
					return err
				}
				var users int
				if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM user").Scan(&users); err != nil {
					return err
				}
				if users != 0 {
					return errors.New("setup published an admin before its write")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				t.Fatalf("Create published before the held writer released: %v", err)
			default:
			}
		}
		check()
		<-s.hasher.slots
		synctest.Wait()
		if len(s.hasher.slots) != 1 || len(s.hasher.waiters) != 0 {
			t.Fatal("Create did not reach the write wait after hashing")
		}
		check()
		finishWriter()
		if err := <-writer; err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		<-s.hasher.slots
		state, err := s.StateForPassword(ctx)
		if err != nil || state.Open || !state.Matches(Verifier("synthetic-capacity-setup")) {
			t.Fatal("setup did not atomically publish its consumed state")
		}
		if err := db.Read(ctx, func(tx *sql.Tx) error {
			var users int
			if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM user").Scan(&users); err != nil {
				return err
			}
			if users != 1 {
				return errors.New("consumed setup did not publish exactly one admin")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if len(s.hasher.passwords) != 0 || len(s.hasher.readTurn) != 0 || len(s.hasher.waiters) != 0 || len(s.hasher.slots) != 0 || len(s.turn) != 0 {
			t.Fatal("Create completion retained admission state")
		}
	})
}

func TestPasswordReadPreservesSQLFailure(t *testing.T) {
	db := openStore(t, t.TempDir())
	t.Cleanup(func() { _ = db.Close() })
	h := NewHasher()
	err := h.readPassword(t.Context(), db, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "SELECT missing_password_column FROM users")
		return err
	})
	if err == nil || errors.Is(err, ErrHashBusy) {
		t.Fatalf("unrelated SQL failure=%v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sentinel := errors.New("unrelated SQL failure")
	err = h.readPassword(ctx, db, func(context.Context, *sql.Tx) error { cancel(); return sentinel })
	if !errors.Is(err, sentinel) || errors.Is(err, ErrHashBusy) {
		t.Fatalf("SQL failure concurrent with cancellation=%v", err)
	}
	if len(h.passwords) != 0 || len(h.readTurn) != 0 {
		t.Fatal("SQL failure retained admission")
	}
}
