//go:build !linux

package install

import "io"

// OpenMountinfo exists so a proof compiles on every target; no mount table is
// read off Linux.
var OpenMountinfo func() (io.ReadCloser, error)

// CheckMount accepts every directory: the filesystem refusal reads
// /proc/self/mountinfo and exists on Linux only. Compile-only here; the gap is
// recorded in docs/portability.md.
func CheckMount(string) error { return nil }
