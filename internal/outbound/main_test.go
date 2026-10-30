package outbound

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/fixture"
)

// escapeEnv marks the child process of TestNetworkEscapeFailsPackage.
const escapeEnv = "BOXSET_FIXTURE_NETWORK_ESCAPE"

// TestMain installs the offline guard: a test of this package that reaches for the
// network fails in-process instead of passing on whatever the host resolves.
func TestMain(m *testing.M) { os.Exit(fixture.NoNetwork(m)) }

// TestNetworkEscapeFailsPackage proves an escape is a failure, not a fallback: the
// child is a provider test with the recorded set installed, it asks for a page the set
// does not hold, and it fails because of that refusal rather than because a name
// happened not to resolve.
func TestNetworkEscapeFailsPackage(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("the test binary: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	child := exec.CommandContext(ctx, binary, "-test.run=^TestNetworkEscapeChild$")
	child.Env = append(os.Environ(), escapeEnv+"=1")
	out, err := child.CombinedOutput()
	if err == nil {
		t.Fatalf("the escaping child passed: a request outside the recorded set was answered\n%s", out)
	}
	if !bytes.Contains(out, []byte("network escape: "+ReasonOutcomeUnknown)) {
		t.Fatalf("the child failed without naming the fixture's refusal\n%s", out)
	}
}

// TestNetworkEscapeChild is the escaping test itself: a provider test whose only
// fixture is the committed set, asking for a page the set does not hold. It has to
// fail, and it names the refusal so its parent can tell this failure from an ambient
// one. Only the isolated child runs it.
func TestNetworkEscapeChild(t *testing.T) {
	if os.Getenv(escapeEnv) != "1" {
		t.Skip("the escape proof runs this test in an isolated child")
	}
	set := loadCommitted(t)
	// At the transport: the set's own refusal, and no dial behind it.
	replay := fixture.NewReplayer(set.doc)
	request, err := http.NewRequest(http.MethodGet, set.address("/escape"), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if _, err := replay.Transport(nil).RoundTrip(request); !errors.Is(err, fixture.ErrUnrecorded) {
		t.Fatalf("the replay gave %v, want %v", err, fixture.ErrUnrecorded)
	}
	// Through the client, the way the provider test calls it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := New(Config{
		Resolver:  rejectingResolver{},
		Slot:      newSlot(1).take,
		Version:   "1.2.3",
		Transport: fixture.NewReplayer(set.doc).Transport,
	})
	defer func() { _ = client.Close(ctx) }()
	_, err = client.Do(ctx, set.op(t, "/escape"), nil)
	if err == nil {
		t.Fatal("network escape: a request the set does not hold was answered")
	}
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Reason != ReasonOutcomeUnknown || refusal.Attempts != 1 {
		t.Fatalf("network escape: %v, want the fixture's refusal", err)
	}
	t.Fatalf("network escape: %s", ReasonOutcomeUnknown)
}

// TestNoNetworkRefusesAnUnfixturedTest proves the guard itself: a test that builds the
// client the way a provider test does — no replay installed — cannot reach a peer, so
// the ordinary suite fails in-process instead of relying on the container's network.
// The guard resolves nothing, so the client's own dial validation refuses the
// destination it could not authorize.
func TestNoNetworkRefusesAnUnfixturedTest(t *testing.T) {
	if _, err := net.DefaultResolver.LookupNetIP(context.Background(), "ip", "provider.example"); !strings.Contains(err.Error(), fixture.ErrNoNetwork.Error()) {
		t.Fatalf("the guard answered a lookup: %v", err)
	}
	client := New(Config{
		Slot:    func(context.Context) (func(), error) { return func() {}, nil },
		Version: "1.2.3",
	})
	defer func() { _ = client.Close(context.Background()) }()
	_, err := client.Do(context.Background(), Op{
		Name:   "unfixtured",
		Target: &Target{ID: "unfixtured", Origin: Origin{Scheme: "http", Host: "provider.example", Port: 8080}},
		Method: http.MethodGet,
		URL:    mustURL(t, "http://provider.example:8080/catalog"),
		Safe:   true,
	}, nil)
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Reason != ReasonDialDenied {
		t.Fatalf("an unfixtured client gave %v, want the dial refusal", err)
	}
}
