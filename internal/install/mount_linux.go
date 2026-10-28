//go:build linux

package install

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// OpenMountinfo opens the kernel's table of mounts. A proof replaces it to
// present a mount the dev container cannot make.
var OpenMountinfo = func() (io.ReadCloser, error) { return os.Open("/proc/self/mountinfo") }

// CheckMount refuses a data directory that lies on a filesystem whose locks
// or flushes Boxset cannot rely on (network shares, FUSE, shared folders of a
// virtual machine). The filesystem type comes from /proc/self/mountinfo; a
// type that is not listed is a local filesystem and is accepted. The directory
// is judged by its resolved spelling (see ResolveDir), so a symlink or a
// directory that does not exist yet is judged by where it really lives.
func CheckMount(dir string) (err error) {
	abs, err := ResolveDir(dir)
	if err != nil {
		return err
	}
	f, err := OpenMountinfo()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	return checkMountinfo(f, abs)
}

// mountEntry is the part of one mountinfo line the lookup needs.
type mountEntry struct {
	id, parent int
	point      string
	fstype     string
}

// checkMountinfo applies the refusal list to the filesystem that serves path
// according to mountinfo. The mount is found the way the kernel resolves the
// path, by walking the mount tree from the root: at each leading part of path
// a mount whose parent is the current one and whose mount point is that part
// replaces the current mount, so a mount covered by another one (a stack on one
// point, or a directory hidden under a later mount of an ancestor) is never the
// answer, whatever its line order or its path length.
func checkMountinfo(mountinfo io.Reader, path string) error {
	var entries []mountEntry
	ids := map[int]bool{}
	sc := bufio.NewScanner(mountinfo)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		// id parent major:minor root mount-point options [optional fields...] - fstype source super-options
		fields := strings.Fields(sc.Text())
		sep := -1
		for i, f := range fields {
			if f == "-" {
				sep = i
				break
			}
		}
		if len(fields) < 6 || sep < 6 || sep+1 >= len(fields) {
			return fmt.Errorf("install: unreadable mountinfo line %q", sc.Text())
		}
		id, idErr := strconv.Atoi(fields[0])
		parent, parentErr := strconv.Atoi(fields[1])
		if err := errors.Join(idErr, parentErr); err != nil {
			return fmt.Errorf("install: unreadable mountinfo line %q: %w", sc.Text(), err)
		}
		entries = append(entries, mountEntry{id, parent, mountinfoUnescape.Replace(fields[4]), fields[sep+1]})
		ids[id] = true
	}
	if err := sc.Err(); err != nil {
		return err
	}

	cur, ok := topOfStack(entries, "/", func(e mountEntry) bool { return !ids[e.parent] })
	if !ok {
		return errors.New("install: mountinfo lists no root mount")
	}
	// One step per leading part of path: "/a/b" is visited as "/a", then "/a/b".
	point := ""
	for _, part := range strings.Split(strings.Trim(path, "/"), "/") {
		if part == "" {
			continue
		}
		point += "/" + part
		parent := cur.id
		if m, found := topOfStack(entries, point, func(e mountEntry) bool { return e.parent == parent }); found {
			cur = m
		}
	}
	if fix := refusal(cur.fstype); fix != "" {
		return &MountRefusedError{FS: cur.fstype, Fix: fix}
	}
	return nil
}

// topOfStack finds the mount on point that first selects and then every mount
// stacked on it at the same point (its child on that point), and returns the
// one on top: the one a path lookup reaches.
func topOfStack(entries []mountEntry, point string, first func(mountEntry) bool) (top mountEntry, found bool) {
	pick := first
	for range len(entries) + 1 {
		hit := false
		for _, e := range entries {
			if e.point == point && pick(e) {
				top, found, hit = e, true, true
				break
			}
		}
		if !hit {
			break
		}
		id := top.id
		pick = func(e mountEntry) bool { return e.parent == id }
	}
	return top, found
}

// mountinfoUnescape undoes the octal escapes /proc writes into a mount point.
var mountinfoUnescape = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

// refusal is the plain-language fix for a filesystem type Boxset refuses, or
// empty for a type it accepts.
func refusal(fstype string) string {
	switch {
	case fstype == "nfs", fstype == "nfs4", fstype == "cifs", fstype == "smb3":
		return "Network shares do not give reliable file locks. Keep the data directory on a local disk: a local folder or a Docker named volume."
	case fstype == "9p", fstype == "drvfs":
		return "Windows drive mounts under WSL do not give reliable file locks. Keep the data directory inside the Linux filesystem or in a Docker named volume, not under /mnt."
	case fstype == "fuse", strings.HasPrefix(fstype, "fuse."), fstype == "fuseblk", strings.HasPrefix(fstype, "fuseblk."), fstype == "virtiofs", fstype == "fakeowner":
		return "Shared folders of Docker Desktop and FUSE mounts do not give reliable file locks. Use a Docker named volume instead of a bind mount of a host folder."
	}
	return ""
}
