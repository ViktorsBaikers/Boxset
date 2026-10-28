package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ViktorsBaikers/Boxset/internal/api"
	"github.com/ViktorsBaikers/Boxset/internal/secret"
)

const statusPath = "/api/v1/system/status"

// recoveringWith leaves the installation in the recovering state a start
// reaches when reason names why no credential authenticates.
func recoveringWith(t *testing.T, in *installation, reason string) {
	t.Helper()
	b := *in.s.backend.Load()
	b.Recovering = reason
	in.s.Attach(b)
	in.s.SetState(Recovering)
}

// TestRecoveringStatusServesCurrentCredential requires a recovering
// installation to serve the one request it allows: a current session's and a
// current key's own status read answers 200 with the state and the reason
// nothing else works, so the recovering state is not a blanket refusal.
func TestRecoveringStatusServesCurrentCredential(t *testing.T) {
	in, cookie := signedIn(t, "")
	_, key := keyFor(t, in, "admin")
	recoveringWith(t, in, secret.ReasonMissing)
	for _, tc := range []struct{ name, header, value string }{
		{"session", "Cookie", cookie.Name + "=" + cookie.Value},
		{"key", "X-Api-Key", key},
	} {
		w := request(in.h, "GET", statusPath, "", tc.header, tc.value)
		if w.Code != 200 {
			t.Fatalf("%s status=%d body=%s", tc.name, w.Code, w.Body.String())
		}
		var got api.Status
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s body %q: %v", tc.name, w.Body.String(), err)
		}
		if got.State != string(Recovering) || got.Reason != secret.ReasonMissing {
			t.Fatalf("%s state=%q reason=%q, want %q and %q", tc.name, got.State, got.Reason, Recovering, secret.ReasonMissing)
		}
	}
}

// TestRecoveringStatusRefusesPendingRestore requires a pending restore to serve
// no credential at or below its epoch. The refusal precedes the credential read,
// so a current session and a current key both answer 401 unauthenticated, and a
// well-formed unknown key answers unauthenticated here while the missing-key
// state answers credential_invalid for it. The control serves those same two
// credentials in this installation, so the refusals are the pending epoch and
// not an invalid credential.
func TestRecoveringStatusRefusesPendingRestore(t *testing.T) {
	in, cookie := signedIn(t, "")
	_, key := keyFor(t, in, "admin")
	unknown := "bxk_" + strings.Repeat("A", 43)
	served := []struct{ name, header, value string }{
		{"session", "Cookie", cookie.Name + "=" + cookie.Value},
		{"key", "X-Api-Key", key},
	}
	recoveringWith(t, in, secret.ReasonMissing)
	for _, tc := range served {
		if w := request(in.h, "GET", statusPath, "", tc.header, tc.value); w.Code != 200 {
			t.Fatalf("control %s under %s status=%d body=%s", tc.name, secret.ReasonMissing, w.Code, w.Body.String())
		}
	}
	if w := request(in.h, "GET", statusPath, "", "X-Api-Key", unknown); w.Code != 401 || code(t, w) != "credential_invalid" {
		t.Fatalf("unknown key under %s status=%d body=%s, want 401 credential_invalid", secret.ReasonMissing, w.Code, w.Body.String())
	}
	recoveringWith(t, in, ReasonRestoreInProgress)
	for _, tc := range served {
		if w := request(in.h, "GET", statusPath, "", tc.header, tc.value); w.Code != 401 || code(t, w) != "unauthenticated" {
			t.Fatalf("%s under %s status=%d body=%s, want 401 unauthenticated", tc.name, ReasonRestoreInProgress, w.Code, w.Body.String())
		}
	}
	if w := request(in.h, "GET", statusPath, "", "X-Api-Key", unknown); w.Code != 401 || code(t, w) != "unauthenticated" {
		t.Fatalf("unknown key under %s status=%d body=%s, want 401 unauthenticated: the refusal precedes the read", ReasonRestoreInProgress, w.Code, w.Body.String())
	}
}

// TestRecoveringStatusRefusesPendingRestoreWhileDraining requires that refusal in
// draining too: draining is no reason to serve a credential the pending restore's
// epoch is about to pass, so a current session and a current key both answer 401
// unauthenticated. The control serves those same two credentials while draining
// without a pending restore, so the refusal is the journal and not the drain.
func TestRecoveringStatusRefusesPendingRestoreWhileDraining(t *testing.T) {
	in, cookie := signedIn(t, "")
	_, key := keyFor(t, in, "admin")
	served := []struct{ name, header, value string }{
		{"session", "Cookie", cookie.Name + "=" + cookie.Value},
		{"key", "X-Api-Key", key},
	}
	in.s.SetState(Draining)
	for _, tc := range served {
		if w := request(in.h, "GET", statusPath, "", tc.header, tc.value); w.Code != 200 {
			t.Fatalf("control %s while draining status=%d body=%s", tc.name, w.Code, w.Body.String())
		}
	}
	recoveringWith(t, in, ReasonRestoreInProgress)
	in.s.SetState(Draining)
	for _, tc := range served {
		if w := request(in.h, "GET", statusPath, "", tc.header, tc.value); w.Code != 401 || code(t, w) != "unauthenticated" {
			t.Fatalf("%s while draining over a pending restore status=%d body=%s, want 401 unauthenticated", tc.name, w.Code, w.Body.String())
		}
	}
}

// TestRecoveringStatusKeyReadFailure requires a key read that fails for any reason
// other than an invalid credential to be this installation's failure, not the
// caller's: the recovering status answers 500 internal_error exactly as every
// other protected route maps the same read.
func TestRecoveringStatusKeyReadFailure(t *testing.T) {
	in, _ := signedIn(t, "")
	_, key := keyFor(t, in, "admin")
	recoveringWith(t, in, secret.ReasonMissing)
	if err := in.db.Close(); err != nil {
		t.Fatal(err)
	}
	if w := request(in.h, "GET", statusPath, "", "X-Api-Key", key); w.Code != 500 || code(t, w) != "internal_error" {
		t.Fatalf("a failed key read status=%d body=%s, want 500 internal_error", w.Code, w.Body.String())
	}
}
