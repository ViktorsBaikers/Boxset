package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RunStateFileName is the file under the data directory that `boxset healthcheck`
// reads: the last lifecycle state this installation recorded.
var RunStateFileName = filepath.Join("run", "state")

// ErrRunState: the run-state file is absent or does not name a lifecycle state.
var ErrRunState = errors.New("server: run_state_invalid")

// Valid reports whether s is one of the four lifecycle states.
func (s State) Valid() bool {
	switch s {
	case Starting, Recovering, Ready, Draining:
		return true
	}
	return false
}

// WriteRunState records st for `boxset healthcheck`. The file is replaced
// atomically, so a reader never sees half a state word and never reads an earlier
// installation's leftover as this one's.
func WriteRunState(dir string, st State) error {
	if !st.Valid() {
		return fmt.Errorf("%w: %q", ErrRunState, st)
	}
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(RunStateFileName)), 0o700); err != nil {
		return fmt.Errorf("%w: %v", ErrRunState, err)
	}
	path := filepath.Join(dir, RunStateFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(st), 0o600); err != nil {
		return fmt.Errorf("%w: %v", ErrRunState, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("%w: %v", ErrRunState, err)
	}
	return nil
}

// ReadRunState returns the state the installation last recorded. A missing file
// wraps fs.ErrNotExist: no server has ever started here.
func ReadRunState(dir string) (State, error) {
	body, err := os.ReadFile(filepath.Join(dir, RunStateFileName))
	if err != nil {
		return "", err
	}
	st := State(strings.TrimSpace(string(body)))
	if !st.Valid() {
		return "", fmt.Errorf("%w: %q", ErrRunState, st)
	}
	return st, nil
}
