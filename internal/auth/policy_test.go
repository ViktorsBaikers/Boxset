package auth

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	storedb "github.com/ViktorsBaikers/Boxset/internal/store/db"
)

func TestCredentialMetadataBatch(t *testing.T) {
	s := sessionFixture(t, "owner")
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	identity, token, _, err := s.Login(t.Context(), "owner", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	keys := s.Keys()
	_, key, err := keys.Create(t.Context(), "OWNER", "automation", "read")
	if err != nil {
		t.Fatal(err)
	}
	readExpiry := func() int64 {
		t.Helper()
		var expiry int64
		if err := s.db.Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRow("SELECT expires_at FROM session WHERE id=?", identity.ID).Scan(&expiry)
		}); err != nil {
			t.Fatal(err)
		}
		return expiry
	}
	initial := readExpiry()
	now = now.Add(time.Hour)
	before := s.db.Commits()
	if got, err := s.AuthenticateReadOnly(t.Context(), token); err != nil || !got.ExpiresAt.Equal(now.Add(SessionTTL)) {
		t.Fatal("read-only sliding expiry", err)
	}
	if _, err := keys.Authenticate(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	if s.db.Commits() != before || readExpiry() != initial {
		t.Fatal("authentication committed metadata")
	}
	if err := s.FlushMetadata(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s.db.Commits() != before+1 || readExpiry() != now.Add(SessionTTL).Unix() {
		t.Fatal("combined batch did not persist useful expiry")
	}
	now = now.Add(time.Second)
	if _, err := s.Authenticate(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushMetadata(t.Context()); err != nil || s.db.Commits() != before+1 {
		t.Fatal("minute limit", err)
	}
	if err := s.Logout(t.Context(), identity); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateReadOnly(t.Context(), token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatal("logout revived", err)
	}
}

func TestScopeIntersection(t *testing.T) {
	s := sessionFixture(t, "owner")
	k := s.Keys()
	for _, scope := range []string{"read", "write", "admin"} {
		id, token, err := k.Create(t.Context(), "owner", scope, scope)
		if err != nil {
			t.Fatal(err)
		}
		p, err := k.Authenticate(t.Context(), token)
		if err != nil || !p.Has(RightUsersRead) || p.Has(RightUsersWrite) != (scope != "read") {
			t.Fatal("admin scope", err)
		}
		if err := s.db.Write(t.Context(), func(tx *sql.Tx) error { _, e := tx.Exec("UPDATE user SET role='viewer'"); return e }); err != nil {
			t.Fatal(err)
		}
		p, err = k.Authenticate(t.Context(), token)
		if err != nil || !p.Has(RightStatusRead) || p.Has(RightUsersRead) || p.Has(RightUsersWrite) {
			t.Fatal("current role intersection", err)
		}
		if err := k.Revoke(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		if _, err := k.Authenticate(t.Context(), token); !errors.Is(err, ErrKeyInvalid) {
			t.Fatal("revoked key accepted", err)
		}
		if err := s.db.Write(t.Context(), func(tx *sql.Tx) error { _, e := tx.Exec("UPDATE user SET role='admin'"); return e }); err != nil {
			t.Fatal(err)
		}
	}
}

// TestActorRecheckDecidesTheRoutesRight requires the use-time recheck to decide
// the right its calling route authorizes, never a fixed one: a credential that
// holds users:write is still refused for a right its route needs and its role
// never grants, and a credential that holds users:read without users:write is
// admitted for the right it does hold.
func TestActorRecheckDecidesTheRoutesRight(t *testing.T) {
	s := sessionFixture(t, "owner")
	owner, _, _, err := s.Login(t.Context(), "owner", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	_, readKey, err := s.Keys().Create(t.Context(), "owner", "reports", "read")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := s.Keys().Authenticate(t.Context(), readKey)
	if err != nil {
		t.Fatal(err)
	}
	if !reader.Has(RightUsersRead) || reader.Has(RightUsersWrite) {
		t.Fatalf("read-scope key rights = %v, want users:read without users:write", reader.Rights)
	}
	recheck := func(session Session, right string) error {
		t.Helper()
		now := s.now().UTC()
		return s.db.Write(t.Context(), func(tx *sql.Tx) error {
			return ActorOf(session).recheck(t.Context(), storedb.New(tx), right, now)
		})
	}
	if err := recheck(owner, RightSetupCreate); !errors.Is(err, ErrForbidden) {
		t.Fatalf("recheck for a right the route needs and no role grants = %v, want ErrForbidden", err)
	}
	if err := recheck(reader, RightUsersRead); err != nil {
		t.Fatalf("recheck for the right the read-scope route needs = %v, want nil", err)
	}
	if err := recheck(reader, RightUsersWrite); !errors.Is(err, ErrForbidden) {
		t.Fatalf("recheck for a right the read scope never held = %v, want ErrForbidden", err)
	}
	// A right withdrawn from the role while the request waited is refused for
	// the right the route needs, not for users:write.
	if err := s.db.Write(t.Context(), func(tx *sql.Tx) error { _, e := tx.Exec("UPDATE user SET role='viewer'"); return e }); err != nil {
		t.Fatal(err)
	}
	if err := recheck(reader, RightUsersRead); !errors.Is(err, ErrForbidden) {
		t.Fatalf("recheck after the role lost users:read = %v, want ErrForbidden", err)
	}
}

// TestSessionsUpdateUserRechecksUsersWrite requires the production edit route to
// supply users:write to its use-time recheck: a read-scope credential that holds
// users:read without users:write is refused, and a write-scope credential commits
// the same edit.
func TestSessionsUpdateUserRechecksUsersWrite(t *testing.T) {
	s := sessionFixture(t, "owner")
	target, err := CreateUser(t.Context(), s.db, NewHasher(), "control", "synthetic-valid-password", RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	keys := s.Keys()
	_, readKey, err := keys.Create(t.Context(), "owner", "reports", "read")
	if err != nil {
		t.Fatal(err)
	}
	_, writeKey, err := keys.Create(t.Context(), "owner", "desk", "write")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := keys.Authenticate(t.Context(), readKey)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := keys.Authenticate(t.Context(), writeKey)
	if err != nil {
		t.Fatal(err)
	}
	if !reader.Has(RightUsersRead) || reader.Has(RightUsersWrite) || !writer.Has(RightUsersWrite) {
		t.Fatalf("key rights read=%v write=%v, want users:read without users:write and users:write", reader.Rights, writer.Rights)
	}
	role := RoleViewer
	patch := UserPatch{ExpectedRevision: target.Revision, Role: &role}
	if _, err := s.UpdateUser(t.Context(), ActorOf(reader), target.ID, patch); !errors.Is(err, ErrForbidden) {
		t.Fatalf("edit by a read-scope key = %v, want ErrForbidden", err)
	}
	// The write-scope control proves the refusal above is the right, not the
	// route refusing every credential. It carries the same expected revision, so
	// a denied request that had committed anything would conflict here.
	updated, err := s.UpdateUser(t.Context(), ActorOf(writer), target.ID, patch)
	if err != nil {
		t.Fatalf("edit by a write-scope key = %v, want nil", err)
	}
	if updated.Role != RoleViewer || updated.Revision != target.Revision+1 {
		t.Fatalf("committed edit = %+v, want role=viewer revision=%d", updated, target.Revision+1)
	}
}

func metadataValues(t *testing.T, s *Sessions) (int64, int64) {
	t.Helper()
	var expiry, used int64
	if err := s.db.Read(t.Context(), func(tx *sql.Tx) error {
		if err := tx.QueryRow("SELECT expires_at FROM session LIMIT 1").Scan(&expiry); err != nil {
			return err
		}
		return tx.QueryRow("SELECT last_used_at FROM api_key LIMIT 1").Scan(&used)
	}); err != nil {
		t.Fatal(err)
	}
	return expiry, used
}

func TestCredentialMetadataFailureRetry(t *testing.T) {
	for _, point := range []string{"before_write", "after_statement", "before_commit"} {
		t.Run(point, func(t *testing.T) {
			s := sessionFixture(t, "owner")
			now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			s.now = func() time.Time { return now }
			_, token, _, err := s.Login(t.Context(), "owner", "correct horse battery")
			if err != nil {
				t.Fatal(err)
			}
			keys := s.Keys()
			_, key, err := keys.Create(t.Context(), "owner", "automation", "read")
			if err != nil {
				t.Fatal(err)
			}
			initialExpiry, initialUse := metadataValues(t, s)
			now = now.Add(time.Hour)
			if _, err := s.AuthenticateReadOnly(t.Context(), token); err != nil {
				t.Fatal(err)
			}
			if _, err := keys.Authenticate(t.Context(), key); err != nil {
				t.Fatal(err)
			}
			before := s.db.Commits()
			watermark := s.lastFlush
			fault := errors.New("synthetic_metadata_fault")
			s.batchFault = func(at string) error {
				if at == point {
					return fault
				}
				return nil
			}
			if _, err := s.Authenticate(t.Context(), token); !errors.Is(err, fault) {
				t.Fatal("direct flush did not propagate fault", err)
			}
			expiry, used := metadataValues(t, s)
			if expiry != initialExpiry || used != initialUse || s.db.Commits() != before || !s.lastFlush.Equal(watermark) || len(s.expiry) != 1 || len(s.keys) != 1 {
				t.Fatal("failed transaction published metadata")
			}
			s.batchFault = nil
			if err := s.FlushMetadata(t.Context()); err != nil || s.db.Commits() != before {
				t.Fatal("failed attempt retried within minute", err)
			}
			now = now.Add(time.Minute)
			if err := s.FlushMetadata(t.Context()); err != nil {
				t.Fatal(err)
			}
			expiry, used = metadataValues(t, s)
			if expiry != now.Add(-time.Minute).Add(SessionTTL).Unix() || used != now.Add(-time.Minute).Unix() || s.db.Commits() != before+1 {
				t.Fatal("combined retry lost pending metadata")
			}
		})
	}
}

func TestCommitBudgetFakeClock(t *testing.T) {
	s := sessionFixture(t, "owner")
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var seconds atomic.Int64
	s.now = func() time.Time { return start.Add(time.Duration(seconds.Load()) * time.Second) }
	_, token, _, err := s.Login(t.Context(), "owner", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	keys := s.Keys()
	_, key, err := keys.Create(t.Context(), "owner", "automation", "admin")
	if err != nil {
		t.Fatal(err)
	}
	before := s.db.Commits()
	var lastCommit, lastExpiry int64
	var priorExpiry int64
	for second := int64(0); second <= 2*3600+120; second++ {
		seconds.Store(second)
		if _, err := keys.Authenticate(t.Context(), key); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AuthenticateReadOnly(t.Context(), token); err != nil {
			t.Fatal(err)
		}
		count := s.db.Commits()
		if second%60 == 0 {
			var wg sync.WaitGroup
			errors := make(chan error, 3)
			wg.Go(func() { errors <- s.FlushMetadata(t.Context()) })
			wg.Go(func() { _, err := s.Authenticate(t.Context(), token); errors <- err })
			wg.Go(func() {
				_, err := keys.Authenticate(t.Context(), key)
				if err == nil {
					err = s.FlushMetadata(t.Context())
				}
				errors <- err
			})
			wg.Wait()
			close(errors)
			for err := range errors {
				if err != nil {
					t.Fatal(err)
				}
			}
		}
		if s.db.Commits() > count {
			if lastCommit != 0 && second-lastCommit < 60 {
				t.Fatal("aggregate commits less than 60 seconds apart")
			}
			lastCommit = second
			expiry, used := metadataValues(t, s)
			if used != start.Add(time.Duration(second)*time.Second).Unix() {
				t.Fatal("last-used batch not useful")
			}
			if priorExpiry != 0 && expiry != priorExpiry {
				if lastExpiry != 0 && second-lastExpiry < 3600 {
					t.Fatal("session persisted twice in hour")
				}
				lastExpiry = second
			}
			priorExpiry = expiry
		}
		if second == 600 && s.db.Commits()-before > 10 {
			t.Fatal("ten-minute workload exceeds commit budget")
		}
	}
	if s.db.Commits()-before != 122 {
		t.Fatalf("actual aggregate commits=%d want=122", s.db.Commits()-before)
	}
	expiry, _ := metadataValues(t, s)
	if expiry < start.Add(2*time.Hour+SessionTTL).Unix() {
		t.Fatal("hourly expiry never persisted")
	}
	t.Logf("COMMIT-BUDGET seconds=7320 commits=%d ten_minute_max=10 minimum_spacing=60", s.db.Commits()-before)
}

func TestCredentialMetadataMergeAndIdentity(t *testing.T) {
	for _, action := range []string{"newer_activity", "logout", "revoke", "epoch"} {
		t.Run(action, func(t *testing.T) {
			s := sessionFixture(t, "owner")
			start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			var seconds atomic.Int64
			s.now = func() time.Time { return start.Add(time.Duration(seconds.Load()) * time.Second) }
			identity, token, _, err := s.Login(t.Context(), "owner", "correct horse battery")
			if err != nil {
				t.Fatal(err)
			}
			keys := s.Keys()
			id, key, err := keys.Create(t.Context(), "owner", "automation", "read")
			if err != nil {
				t.Fatal(err)
			}
			seconds.Store(3600)
			if _, err := s.AuthenticateReadOnly(t.Context(), token); err != nil {
				t.Fatal(err)
			}
			if _, err := keys.Authenticate(t.Context(), key); err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			s.batchFault = func(point string) error {
				if point == "before_write" {
					close(entered)
					<-release
				}
				return nil
			}
			done := make(chan error, 1)
			go func() { done <- s.FlushMetadata(t.Context()) }()
			<-entered
			seconds.Store(3630)
			switch action {
			case "newer_activity":
				if _, err := s.AuthenticateReadOnly(t.Context(), token); err != nil {
					t.Fatal(err)
				}
				if _, err := keys.Authenticate(t.Context(), key); err != nil {
					t.Fatal(err)
				}
			case "logout":
				if err := s.Logout(t.Context(), identity); err != nil {
					t.Fatal(err)
				}
			case "revoke":
				if err := keys.Revoke(t.Context(), id); err != nil {
					t.Fatal(err)
				}
			case "epoch":
				if err := s.db.Write(t.Context(), func(tx *sql.Tx) error {
					_, err := tx.Exec("UPDATE installation SET restore_epoch=restore_epoch+1")
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			s.batchFault = nil
			switch action {
			case "newer_activity":
				v := Verifier(token)
				if !s.expiry[hex.EncodeToString(v[:])].expires.Equal(start.Add(3630*time.Second+SessionTTL)) || len(s.keys) != 1 {
					t.Fatal("commit merge lost newer activity")
				}
				seconds.Store(3690)
				if err := s.FlushMetadata(t.Context()); err != nil {
					t.Fatal(err)
				}
				_, used := metadataValues(t, s)
				if used != start.Add(3630*time.Second).Unix() {
					t.Fatal("newer key activity not retried")
				}
			case "logout":
				if _, err := s.AuthenticateReadOnly(t.Context(), token); !errors.Is(err, ErrSessionInvalid) || len(s.expiry) != 0 {
					t.Fatal("logout metadata resurrected")
				}
			case "revoke":
				if _, err := keys.Authenticate(t.Context(), key); !errors.Is(err, ErrKeyInvalid) || len(s.keys) != 0 {
					t.Fatal("revoked metadata resurrected")
				}
			case "epoch":
				if _, err := s.AuthenticateReadOnly(t.Context(), token); !errors.Is(err, ErrSessionInvalid) {
					t.Fatal("old session epoch")
				}
				if _, err := keys.Authenticate(t.Context(), key); !errors.Is(err, ErrKeyInvalid) {
					t.Fatal("old key epoch")
				}
				_, fresh, _, err := s.Login(t.Context(), "owner", "correct horse battery")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.AuthenticateReadOnly(t.Context(), fresh); err != nil {
					t.Fatal("current epoch control", err)
				}
			}
		})
	}
}

func TestCredentialMetadataBoundsAndFairness(t *testing.T) {
	s := sessionFixture(t, "owner")
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	// Seed real credentials without password work; the same public authenticators
	// and generated query boundary process every one afterward.
	tokens, keys := make([]string, 4097), make([]string, 4097)
	if err := s.db.Write(t.Context(), func(tx *sql.Tx) error {
		q := storedb.New(tx)
		for i := range tokens {
			tokens[i] = fmt.Sprintf("%043d", i)
			keys[i] = "bxk_" + tokens[i]
			v := Verifier(tokens[i])
			if _, err := q.CreateSession(t.Context(), storedb.CreateSessionParams{TokenSha256: hex.EncodeToString(v[:]), UserID: 1, Epoch: 1, CreatedAt: now.Unix(), ExpiresAt: now.Add(SessionTTL).Unix()}); err != nil {
				return err
			}
			v = Verifier(keys[i])
			if _, err := q.CreateAPIKey(t.Context(), storedb.CreateAPIKeyParams{TokenSha256: hex.EncodeToString(v[:]), UserID: 1, Name: "automation", Scope: "read", Epoch: 1, CreatedAt: now.Unix()}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	k := s.Keys()
	for i := range 4096 {
		if _, err := s.AuthenticateReadOnly(t.Context(), tokens[i]); err != nil {
			t.Fatal(err)
		}
		if _, err := k.Authenticate(t.Context(), keys[i]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AuthenticateReadOnly(t.Context(), tokens[4096]); err == nil || err.Error() != "session_capacity" {
		t.Fatal("session cap+1")
	}
	if p, err := k.Authenticate(t.Context(), keys[4096]); err != nil || !p.Has(RightStatusRead) {
		t.Fatal("metadata cap bypassed key verification", err)
	}
	if len(s.expiry) != 4096 || len(s.keys) != 4096 || metadataReservedBytes > metadataByteLimit {
		t.Fatal("metadata reservation bound")
	}
	now = now.Add(time.Hour)
	for i := range 4096 {
		if _, err := s.AuthenticateReadOnly(t.Context(), tokens[i]); err != nil {
			t.Fatal(err)
		}
	}
	before := s.db.Commits()
	for minute := range 32 {
		if minute > 0 {
			now = now.Add(time.Minute)
		}
		if err := s.FlushMetadata(t.Context()); err != nil {
			t.Fatal(err)
		}
		var updated int
		if err := s.db.Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRow("SELECT (SELECT count(*) FROM api_key WHERE last_used_at>0)+(SELECT count(*) FROM session WHERE expires_at>?)", time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC).Unix()).Scan(&updated)
		}); err != nil {
			t.Fatal(err)
		}
		if updated != (minute+1)*256 {
			t.Fatalf("bounded fair batch minute=%d updated=%d", minute, updated)
		}
	}
	if s.db.Commits() != before+32 || len(s.keys) != 0 {
		t.Fatal("eligible credentials starved")
	}
	now = now.Add(time.Hour)
	for i := range 4096 {
		if _, err := s.AuthenticateReadOnly(t.Context(), tokens[i]); err != nil {
			t.Fatal(err)
		}
	}
	before = s.db.Commits()
	for minute := range 32 {
		if minute > 0 {
			now = now.Add(time.Minute)
		}
		for i := range 4096 {
			if _, err := k.Authenticate(t.Context(), keys[i]); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.FlushMetadata(t.Context()); err != nil {
			t.Fatal(err)
		}
		var updated int
		if err := s.db.Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRow("SELECT (SELECT count(*) FROM api_key WHERE last_used_at>?)+(SELECT count(*) FROM session WHERE expires_at>?)", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Unix(), time.Date(2026, 10, 31, 1, 0, 0, 0, time.UTC).Unix()).Scan(&updated)
		}); err != nil {
			t.Fatal(err)
		}
		if updated != (minute+1)*256 {
			t.Fatalf("sustained key traffic starved pending metadata: minute=%d updated=%d", minute, updated)
		}
	}
	if s.db.Commits() != before+32 {
		t.Fatal("sustained workload exceeded aggregate commit budget")
	}
	now = now.Add(SessionTTL)
	if _, err := s.AuthenticateReadOnly(t.Context(), tokens[4096]); !errors.Is(err, ErrSessionInvalid) {
		t.Fatal("expired credential")
	}
	if err := s.FlushMetadata(t.Context()); err != nil || len(s.expiry) != 0 {
		t.Fatal("expired entries not reclaimed", err)
	}
	t.Logf("METADATA-BOUNDS sessions=4096 keys=4096 reserved_bytes=%d batch_max=256 initial_fair_batches=32 sustained_fair_batches=32", metadataReservedBytes)
}

func TestCredentialMetadataCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := sessionFixture(t, "owner")
		now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		s.now = func() time.Time { return now }
		_, token, _, err := s.Login(t.Context(), "owner", "correct horse battery")
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Hour)
		if _, err := s.AuthenticateReadOnly(t.Context(), token); err != nil {
			t.Fatal(err)
		}
		for _, point := range []string{"waiting_writer", "after_statement"} {
			ctx, cancel := context.WithCancel(t.Context())
			var release chan struct{}
			var writerDone chan error
			if point == "waiting_writer" {
				release = make(chan struct{})
				entered := make(chan struct{})
				writerDone = make(chan error, 1)
				go func() {
					writerDone <- s.db.Write(t.Context(), func(*sql.Tx) error { close(entered); <-release; return errors.New("synthetic_writer_rollback") })
				}()
				<-entered
			} else {
				s.batchFault = func(at string) error {
					if at == point {
						cancel()
						return ctx.Err()
					}
					return nil
				}
			}
			done := make(chan error, 1)
			go func() { done <- s.FlushMetadata(ctx) }()
			if point == "waiting_writer" {
				synctest.Wait()
				cancel()
			}
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal("canceled SQL attempt", err)
			}
			if release != nil {
				close(release)
				if err := <-writerDone; err == nil {
					t.Fatal("writer rollback control")
				}
			}
			cancel()
			s.batchFault = nil
			now = now.Add(time.Minute)
		}
		// A waiting flush caller is cancelable and never blocks authentication.
		s.flush <- struct{}{}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- s.FlushMetadata(ctx) }()
		synctest.Wait()
		if _, err := s.AuthenticateReadOnly(t.Context(), token); err != nil {
			t.Fatal(err)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal("admission waiter")
		}
		<-s.flush
		s.flush <- struct{}{}
		bounded := make(chan error, 1)
		go func() { bounded <- s.FlushMetadata(t.Context()) }()
		synctest.Wait()
		time.Sleep(2 * time.Second)
		if err := <-bounded; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("flush admission exceeded attempt deadline", err)
		}
		<-s.flush
		if err := s.FlushMetadata(t.Context()); err != nil {
			t.Fatal("cancellation stranded admission", err)
		}
		now = now.Add(-time.Hour)
		before := s.db.Commits()
		if err := s.FlushMetadata(t.Context()); err != nil || s.db.Commits() != before {
			t.Fatal("backward clock early commit", err)
		}
		s.StopMetadata()
		now = now.Add(2 * time.Hour)
		if _, err := s.AuthenticateReadOnly(t.Context(), token); err != nil {
			t.Fatal(err)
		}
		if err := s.FlushMetadata(t.Context()); err != nil || s.db.Commits() != before {
			t.Fatal("stopped admission committed", err)
		}
	})
}

func TestCredentialMetadataStaggered(t *testing.T) {
	s := sessionFixture(t, "owner")
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	now := start
	s.now = func() time.Time { return now }
	var tokens []string
	for _, second := range []int{0, 30, 59} {
		now = start.Add(time.Duration(second) * time.Second)
		_, token, _, err := s.Login(t.Context(), "owner", "correct horse battery")
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, token)
	}
	k := s.Keys()
	_, key, err := k.Create(t.Context(), "owner", "automation", "read")
	if err != nil {
		t.Fatal(err)
	}
	before := s.db.Commits()
	for i, second := range []int{3600, 3630, 3659, 3660} {
		now = start.Add(time.Duration(second) * time.Second)
		for _, token := range tokens {
			if _, err := s.AuthenticateReadOnly(t.Context(), token); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := k.Authenticate(t.Context(), key); err != nil {
			t.Fatal(err)
		}
		if err := s.FlushMetadata(t.Context()); err != nil {
			t.Fatal(err)
		}
		want := int64(1)
		if i == 3 {
			want = 2
		}
		if s.db.Commits() != before+want {
			t.Fatal("staggered credentials split aggregate cadence")
		}
	}
	var eligible int
	if err := s.db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT count(*) FROM session WHERE expires_at>=?", start.Add(3660*time.Second+SessionTTL).Unix()).Scan(&eligible)
	}); err != nil {
		t.Fatal(err)
	}
	if eligible != 2 {
		t.Fatal("staggered sessions not eventually persisted")
	}
}

func TestCredentialMetadataClockAtPublication(t *testing.T) {
	s := sessionFixture(t, "owner")
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var seconds atomic.Int64
	s.now = func() time.Time { return start.Add(time.Duration(seconds.Load()) * time.Second) }
	k := s.Keys()
	_, key, err := k.Create(t.Context(), "owner", "automation", "read")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Authenticate(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	seconds.Store(3600)
	if _, err := k.Authenticate(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	s.batchFault = func(point string) error {
		if point == "before_write" {
			close(entered)
			<-release
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- s.FlushMetadata(t.Context()) }()
	<-entered
	seconds.Store(3630)
	if _, err := k.Authenticate(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	seconds.Store(3600)
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s.batchFault = nil
	if !s.lastFlush.Equal(start.Add(3630 * time.Second)) {
		t.Fatal("commit publication moved behind observed activity clock")
	}
	before := s.db.Commits()
	seconds.Store(3660)
	if err := s.FlushMetadata(t.Context()); err != nil || s.db.Commits() != before {
		t.Fatal("backward in-flight clock allowed early commit", err)
	}
	seconds.Store(3690)
	if err := s.FlushMetadata(t.Context()); err != nil || s.db.Commits() != before+1 {
		t.Fatal("pending newer activity never persisted", err)
	}
}

func TestHashBurstBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := sessionFixture(t, "owner")
		h := s.hasher
		h.slots <- struct{}{}
		h.slots <- struct{}{}
		errorsOut := make(chan error, 50)
		for range 50 {
			go func() { _, _, _, err := s.Login(t.Context(), "owner", "wrong-password"); errorsOut <- err }()
		}
		synctest.Wait()
		if len(h.slots) != 2 {
			t.Fatal("hash admission exceeded two")
		}
		time.Sleep(2 * time.Second)
		for range 50 {
			if err := <-errorsOut; !errors.Is(err, ErrHashBusy) {
				t.Fatal("burst waiter did not receive bounded refusal", err)
			}
		}
		<-h.slots
		<-h.slots
	})
}

func TestCredentialMetadataDisableDuringFlush(t *testing.T) {
	s := sessionFixture(t, "owner")
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var seconds atomic.Int64
	s.now = func() time.Time { return start.Add(time.Duration(seconds.Load()) * time.Second) }
	_, token, _, err := s.Login(t.Context(), "owner", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	seconds.Store(3600)
	if _, err := s.AuthenticateReadOnly(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	s.batchFault = func(point string) error {
		if point == "before_write" {
			close(entered)
			<-release
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- s.FlushMetadata(t.Context()) }()
	<-entered
	if err := s.db.Write(t.Context(), func(tx *sql.Tx) error { _, err := tx.Exec("UPDATE user SET disabled=1"); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateReadOnly(t.Context(), token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatal("disabled session accepted", err)
	}
	if err := s.db.Write(t.Context(), func(tx *sql.Tx) error { _, err := tx.Exec("UPDATE user SET disabled=0"); return err }); err != nil {
		t.Fatal(err)
	}
	seconds.Store(3601)
	if _, err := s.AuthenticateReadOnly(t.Context(), token); err != nil {
		t.Fatal("reenabled control", err)
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s.batchFault = nil
	before := s.db.Commits()
	seconds.Store(3661)
	if _, err := s.AuthenticateReadOnly(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushMetadata(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s.db.Commits() != before {
		t.Fatal("disable/reenable lost the session hourly watermark")
	}
}

func TestCredentialMetadataSubsecondCadence(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("first_attempt_failed=%v", failed), func(t *testing.T) {
			s := sessionFixture(t, "owner")
			start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			now := start
			s.now = func() time.Time { return now }
			keys := s.Keys()
			_, token, err := keys.Create(t.Context(), "owner", "automation", "read")
			if err != nil {
				t.Fatal(err)
			}
			now = start.Add(900 * time.Millisecond)
			if _, err := keys.Authenticate(t.Context(), token); err != nil {
				t.Fatal(err)
			}
			before := s.db.Commits()
			now = start.Add(60*time.Second + 100*time.Millisecond)
			if err := s.FlushMetadata(t.Context()); err != nil {
				t.Fatal(err)
			}
			if s.db.Commits() != before {
				t.Fatal("fractional cadence admitted before initial minute")
			}
			now = start.Add(60*time.Second + 900*time.Millisecond)
			fault := errors.New("synthetic_attempt_failure")
			if failed {
				s.batchFault = func(string) error { return fault }
			}
			err = s.FlushMetadata(t.Context())
			commits := int64(1)
			if failed {
				commits = 0
				if !errors.Is(err, fault) {
					t.Fatal("failed attempt control", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			s.batchFault = nil
			if s.db.Commits() != before+commits {
				t.Fatal("first eligible attempt commit count")
			}
			now = start.Add(120*time.Second + 100*time.Millisecond)
			if _, err := keys.Authenticate(t.Context(), token); err != nil {
				t.Fatal(err)
			}
			if err := s.FlushMetadata(t.Context()); err != nil {
				t.Fatal(err)
			}
			if s.db.Commits() != before+commits {
				t.Fatal("fractional cadence allowed attempts or commits less than 60 seconds apart")
			}
			now = start.Add(120*time.Second + 900*time.Millisecond)
			if err := s.FlushMetadata(t.Context()); err != nil {
				t.Fatal(err)
			}
			if s.db.Commits() != before+commits+1 {
				t.Fatal("fractional boundary lost queued activity")
			}
		})
	}
}
