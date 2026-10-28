package logs

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/redact"
)

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

// TestRotatingLogCapWhileRunning requires neither file to pass the per-file cap
// at any moment of a burst far larger than the cap, not only when the burst
// ends: a write that would pass the cap rotates first and is then split, so the
// bound holds while the server is running.
func TestRotatingLogCapWhileRunning(t *testing.T) {
	dir := t.TempDir()
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	current := filepath.Join(dir, DirName, "boxset.log")
	rotated := current + ".1"
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	for written := 0; written < 3*(8<<20); written += len(chunk) {
		if _, err := r.Write(chunk); err != nil {
			t.Fatal(err)
		}
		if size := fileSize(t, current); size > 8<<20 {
			t.Fatalf("boxset.log holds %d bytes while running, want at most the recorded %d", size, 8<<20)
		}
		if size := fileSize(t, rotated); size > 8<<20 {
			t.Fatalf("boxset.log.1 holds %d bytes while running, want at most the recorded %d", size, 8<<20)
		}
	}
	if size := fileSize(t, rotated); size == 0 {
		t.Fatal("a burst past the recorded cap left no rotation")
	}
	if size := fileSize(t, current); size == 0 {
		t.Fatal("the current file did not receive the tail of the burst")
	}
	if size := fileSize(t, current) + fileSize(t, rotated); size > 2*(8<<20) {
		t.Fatalf("both files hold %d bytes, want at most the recorded %d", size, 2*(8<<20))
	}
}

// TestRingHandlerRetainsCappedRecords drives the serve chain — the redactor over
// the ring handler over the sink — with a burst far past the cap and requires the
// retained records to stay inside the recorded caps: at most the recorded 2 MiB,
// already redacted by the time they are retained, and an entry past the recorded
// 24 h gone even while the byte cap has room. The clock is injected
// so the age bound costs no wait.
func TestRingHandlerRetainsCappedRecords(t *testing.T) {
	const secret = "synthetic-log-ring-secret"
	now := time.Unix(0, 0)
	retainer := newRingHandler(slog.NewTextHandler(io.Discard, nil), func() time.Time { return now })
	log := slog.New(redact.NewHandler(retainer))

	line := strings.Repeat("x", 4<<10)
	for written := 0; written < 3*(2<<20); written += len(line) {
		log.Info(line, "apikey", secret)
	}
	retained := retainer.Ring().Entries()
	if len(retained) == 0 {
		t.Fatal("a burst past the byte cap retained no record")
	}
	bytesRetained := 0
	for _, entry := range retained {
		bytesRetained += len(entry)
		if bytes.Contains(entry, []byte(secret)) {
			t.Fatal("a secret reached the retained record")
		}
	}
	if bytesRetained > 2<<20 {
		t.Fatalf("the ring retains %d bytes, want at most the recorded %d", bytesRetained, 2<<20)
	}

	now = now.Add(24 * time.Hour)
	log.Info("after the age cap")
	aged := retainer.Ring().Entries()
	if len(aged) != 1 || !bytes.Contains(aged[0], []byte("after the age cap")) {
		t.Fatalf("the age cap left %d records, want only the one logged past it", len(aged))
	}
}

// TestCrashMonitorCapsLive requires the crash record to stay off disk while the
// crashing process is still writing: the monitor drains into memory, retains at
// most the recorded 1 MiB of redacted bytes from a burst far larger than that, and publishes
// once, at EOF.
func TestCrashMonitorCapsLive(t *testing.T) {
	const sentinel = "synthetic-crash-secret"
	dir := t.TempDir()
	published := filepath.Join(dir, DirName, CrashName)
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- Monitor(dir, reader) }()
	burst := []byte(strings.Repeat("panic: synthetic traceback apikey="+sentinel+"\n", 8*(1<<20)/(40+len(sentinel))))
	go func() { _, _ = writer.Write(burst) }()
	// A crash record that is still being written is never on disk.
	time.Sleep(50 * time.Millisecond)
	if size := fileSize(t, published); size != 0 {
		t.Fatalf("%d crash bytes reached disk while the process was still writing", size)
	}
	_, _ = writer.Write([]byte("panic: end\n"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(published)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > 1<<20 {
		t.Fatalf("crash.log holds %d bytes, want at most the recorded %d", len(body), 1<<20)
	}
	if len(body) == 0 {
		t.Fatal("crash.log is empty after a burst")
	}
	if bytes.Contains(body, []byte(sentinel)) {
		t.Fatal("a secret reached the crash record")
	}

	// The next record keeps the one it replaced.
	second := io.NopCloser(strings.NewReader("panic: second\n"))
	if err := Monitor(dir, second); err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(filepath.Join(dir, DirName, CrashPreviousName))
	if err != nil {
		t.Fatal(err)
	}
	if len(previous) != len(body) {
		t.Fatalf("crash.log.1 holds %d bytes, want the %d of the replaced record", len(previous), len(body))
	}
	// An EOF with nothing retained publishes nothing over a real record.
	if err := Monitor(dir, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if ago := fileSize(t, published); ago != len("panic: second\n") {
		t.Fatalf("an empty crash replaced the record with %d bytes", ago)
	}
}
