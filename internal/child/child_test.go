package child

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// helperMode is the argument this test binary reads to act as a child: the
// runner gives a child only the marker environment, so its behaviour has to come
// from its own arguments.
func helperMode() string {
	args := os.Args
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--" {
			return args[i+1]
		}
	}
	return ""
}

// TestHelperProcess is the child every case below runs: this test binary
// re-executed, so no shell, second program or network is involved.
func TestHelperProcess(t *testing.T) {
	mode := helperMode()
	if mode == "" {
		t.Skip("not the child")
	}
	switch mode {
	case "env":
		for _, entry := range os.Environ() {
			_, _ = os.Stdout.WriteString(entry + "\n")
		}
	case "flood":
		// A secret in the flood proves the transcript is redacted, not only capped.
		_, _ = os.Stdout.WriteString("apikey=" + floodSecret + "\n")
		chunk := []byte(strings.Repeat("synthetic traceback line apikey="+floodSecret+"\n", 64))
		for written := 0; written < 20<<20; written += len(chunk) {
			if _, err := os.Stdout.Write(chunk); err != nil {
				return
			}
			if _, err := os.Stderr.Write(chunk); err != nil {
				return
			}
		}
	case "tree":
		// A descendant of the child, and a heartbeat that stops when it dies.
		beacon := os.Args[len(os.Args)-1]
		grandchild := startHeartbeat(beacon)
		if grandchild == nil {
			return
		}
		time.Sleep(time.Minute)
	case "beat":
		beacon := os.Args[len(os.Args)-1]
		for {
			file, err := os.OpenFile(beacon, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				return
			}
			_, _ = file.Write([]byte("x"))
			_ = file.Close()
			time.Sleep(10 * time.Millisecond)
		}
	}
}

const floodSecret = "synthetic-child-secret"

// markerEntries returns the BOXSET_-prefixed environment entries of a transcript.
func markerEntries(output string) []string {
	var marked []string
	for _, entry := range strings.Split(output, "\n") {
		if strings.HasPrefix(entry, "BOXSET_") {
			marked = append(marked, entry)
		}
	}
	return marked
}

// startHeartbeat starts the grandchild the group-kill cases watch.
func startHeartbeat(beacon string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--", "beat", beacon)
	cmd.Env = []string{Marker}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return nil
	}
	return cmd
}

// self returns the arguments that re-execute this test binary as the child in
// mode, with extra appended after it.
func self(mode string, extra ...string) Spec {
	args := append([]string{"-test.run=TestHelperProcess", "--", mode}, extra...)
	return Spec{Path: os.Args[0], Args: args, Timeout: 30 * time.Second, AttemptID: AttemptID{1}}
}

// TestChildEnvAndGroupKill requires a child to see only its explicit environment
// and a run past its timeout to end, with the child's own group killed.
func TestChildEnvAndGroupKill(t *testing.T) {
	res, err := Run(t.Context(), self("env"))
	if err != nil {
		t.Fatal(err)
	}
	// The test binary prints its own verdict beside the environment it saw; the
	// environment entries are the marker and nothing else.
	if marked := markerEntries(res.Output); len(marked) != 1 || marked[0] != Marker {
		t.Fatalf("child environment = %q, want only %q", marked, Marker)
	}

	spec := self("tree", filepath.Join(t.TempDir(), "beacon"))
	spec.Timeout = 300 * time.Millisecond
	res, err = Run(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Fatalf("a child past its 300 ms timeout reported TimedOut=%v exit=%d", res.TimedOut, res.Exit)
	}
	if res.Exit == 0 {
		t.Fatal("a timed-out child reported a successful exit")
	}
}

// TestChildDescendantsKilled requires the timeout to take the child's whole
// process group: a grandchild that writes a heartbeat stops writing once the run
// ends, which a kill of the direct child alone would not achieve.
func TestChildDescendantsKilled(t *testing.T) {
	beacon := filepath.Join(t.TempDir(), "beacon")
	spec := self("tree", beacon)
	spec.Timeout = 500 * time.Millisecond
	if _, err := Run(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	before := fileSize(t, beacon)
	if before == 0 {
		t.Fatal("the descendant never wrote its heartbeat, so the case proves nothing")
	}
	time.Sleep(300 * time.Millisecond)
	if after := fileSize(t, beacon); after != before {
		t.Fatalf("a descendant outlived the timed-out child: %d bytes then %d", before, after)
	}
}

// TestChildEnvNoSecrets requires no parent BOXSET_* setting, and no secret, to
// reach a child's environment.
func TestChildEnvNoSecrets(t *testing.T) {
	const sentinel = "synthetic-environment-secret"
	for _, name := range []string{"BOXSET_SETUP_TOKEN", "BOXSET_DATA_DIR", "BOXSET_LOCAL_BYPASS"} {
		t.Setenv(name, sentinel+"-"+name)
	}
	t.Setenv("BOXSET_SECRET", sentinel)
	res, err := Run(t.Context(), self("env"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Output, sentinel) {
		t.Fatalf("a parent secret reached the child: %q", res.Output)
	}
	for _, entry := range strings.Split(strings.TrimSpace(res.Output), "\n") {
		if entry != Marker && strings.HasPrefix(entry, "BOXSET_") {
			t.Fatalf("parent setting %q reached the child", entry)
		}
	}
}

func fileSize(t *testing.T, path string) int {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return int(info.Size())
}

// TestChildOutputCap requires a flood to be drained to the end (the child
// finishes instead of blocking on a full pipe), retained only up to the cap, and
// redacted.
func TestChildOutputCap(t *testing.T) {
	res, err := Run(t.Context(), self("flood"))
	if err != nil {
		t.Fatal(err)
	}
	if res.TimedOut {
		t.Fatal("the child blocked on its own output instead of being drained")
	}
	if res.Exit != 0 {
		t.Fatalf("flooding child exit = %d", res.Exit)
	}
	if len(res.Output) > 64<<10 {
		t.Fatalf("retained %d bytes, want at most the recorded %d", len(res.Output), 64<<10)
	}
	if len(res.Output) == 0 {
		t.Fatal("retained nothing of a flood")
	}
	if strings.Contains(res.Output, floodSecret) {
		t.Fatal("a secret in the child's output reached the transcript")
	}
}
