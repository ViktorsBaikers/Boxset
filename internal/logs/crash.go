package logs

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ViktorsBaikers/Boxset/internal/redact"
)

// CrashName is the published crash record; CrashPreviousName is the record it
// replaced.
const (
	CrashName         = "crash.log"
	CrashPreviousName = "crash.log.1"
)

// Monitor drains the crash output of a server from r and publishes it as
// <dir>/crash.log once the writing process has ended, which is the moment r
// reaches EOF. Nothing is written to disk before that: a crash record that is
// still being produced is kept in memory, so no reader ever sees half a
// traceback as if it were the record, and a running server costs its crash
// output at most CrashCap of memory and no disk bytes. At most CrashCap redacted
// bytes are retained; the rest is read and discarded so the writer never blocks.
// The record it replaces is kept as crash.log.1.
func Monitor(dir string, r io.Reader) error {
	retained, err := drain(r, CrashCap)
	if err != nil {
		return err
	}
	if len(retained) == 0 {
		return nil
	}
	root := filepath.Join(dir, DirName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("%w: %v", ErrLogDir, err)
	}
	published := filepath.Join(root, CrashName)
	tmp := filepath.Join(root, CrashName+".tmp")
	if err := writeFile(tmp, retained); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(root, CrashPreviousName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(published, filepath.Join(root, CrashPreviousName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(tmp, published)
}

// drain reads all of r, redacting what it retains, and returns at most limit
// bytes. It reads to EOF whatever it retains, so a crash dump larger than the
// cap never blocks the process that is writing it.
//
// ponytail: the retained bytes are the head of the dump, not the tail. The head
// names the fault; keeping both would need a second buffer for no diagnosis.
func drain(r io.Reader, limit int) ([]byte, error) {
	reader := bufio.NewReaderSize(r, 32<<10)
	out := make([]byte, 0, limit)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			redacted := redact.String(string(line))
			if room := limit - len(out); room > 0 {
				if len(redacted) > room {
					redacted = redacted[:room]
				}
				out = append(out, redacted...)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return out, err
		}
	}
}

func writeFile(path string, bytes []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrLogDir, err)
	}
	if _, err := file.Write(bytes); err != nil {
		_ = file.Close()
		return fmt.Errorf("%w: %v", ErrLogDir, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("%w: %v", ErrLogDir, err)
	}
	return file.Close()
}
