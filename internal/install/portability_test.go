//go:build linux

package install

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMountFixtures(t *testing.T) {
	open := func(t *testing.T, name string) *os.File {
		t.Helper()
		f, err := os.Open("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}

	refused := []struct{ path, fs string }{
		{"/mnt/nfs/boxset", "nfs"},
		{"/mnt/nfs4/boxset", "nfs4"},
		{"/mnt/cifs", "cifs"},
		{"/mnt/smb3/boxset/data", "smb3"},
		{"/mnt/fuse-sshfs/boxset", "fuse.sshfs"},
		{"/mnt/fuse/boxset", "fuse"},
		{"/mnt/virtiofs/boxset", "virtiofs"},
		{"/mnt/fakeowner/boxset", "fakeowner"},
		{"/mnt/9p/boxset", "9p"},
		{"/mnt/drvfs/boxset", "drvfs"},
		{"/mnt/my share/boxset", "cifs"}, // the mount point is stored with an octal escape
		{"/top/boxset", "nfs"},           // the mount on top of an earlier one on the same point decides
		{"/hidden/config/boxset", "nfs"}, // a local mount hidden under an NFS mount of its parent directory
		{"/ord/boxset", "nfs"},           // the NFS mount on top decides although its line is listed first
		{"/mnt/fuseblk/boxset", "fuseblk"},
		{"/mnt/fuseblk-sub/boxset", "fuseblk.ntfs"},
	}
	for _, c := range refused {
		err := checkMountinfo(open(t, "mountinfo-refused.txt"), c.path)
		var m *MountRefusedError
		if !errors.As(err, &m) || m.FS != c.fs {
			t.Errorf("%s: err = %v, want a refusal naming fs=%s", c.path, err, c.fs)
			continue
		}
		if m.Fix == "" || !strings.Contains(err.Error(), "fs="+c.fs) || !strings.Contains(err.Error(), m.Fix) {
			t.Errorf("%s: message %q does not name the filesystem and the fix", c.path, err)
		}
		t.Logf("MOUNT-REFUSED %s fs=%s", c.path, c.fs)
	}

	allowed := []string{
		"/", "/home/boxset", "/tmp/boxset", "/var/lib/docker/volumes/v/_data",
		"/data/boxset",
		"/exotic/boxset",       // a local type the list does not name starts
		"/srv/local/boxset",    // a local mount below a network mount point
		"/stack/boxset",        // the later local mount on the same point decides
		"/mnt/nfsbackup/data",  // /mnt/nfs is a different path component
		"/netstore/sub/boxset", // an NFS mount hidden under a later local mount of its parent directory
		"/ordok/boxset",        // the local mount on top decides although its line is listed first
	}
	for _, p := range allowed {
		if err := checkMountinfo(open(t, "mountinfo-allowed.txt"), p); err != nil {
			t.Errorf("%s: err = %v, want it accepted", p, err)
		}
	}

	// A mountinfo line that cannot be read is a failure, never an accepted directory.
	err := checkMountinfo(strings.NewReader("not a mountinfo line\n"), "/data")
	var m *MountRefusedError
	if err == nil || errors.As(err, &m) {
		t.Errorf("malformed mountinfo: err = %v, want a read failure", err)
	}
}

// The directory is judged by where it really lives: through a symlink, below a
// part that does not exist yet, and as an alias-shaped name. The mount table is
// supplied through the same seam the kernel's is read through.
func TestCheckMountResolvesDirectory(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	forbidden := filepath.Join(root, "share")
	if err := os.MkdirAll(forbidden, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(forbidden, link); err != nil {
		t.Fatal(err)
	}
	table := "1 0 8:1 / / rw - ext4 /dev/sda rw\n2 1 0:50 / " + forbidden + " rw - nfs4 srv:/x rw\n"
	realTable := OpenMountinfo
	t.Cleanup(func() { OpenMountinfo = realTable })
	OpenMountinfo = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(table)), nil }

	for name, dir := range map[string]string{
		"existing":          forbidden,
		"missing child":     filepath.Join(forbidden, "a", "b"),
		"through a symlink": filepath.Join(link, "data"),
	} {
		var m *MountRefusedError
		if err := CheckMount(dir); !errors.As(err, &m) || m.FS != "nfs4" {
			t.Errorf("%s: CheckMount(%s) = %v, want a refusal naming nfs4", name, dir, err)
		}
	}
	if err := CheckMount(filepath.Join(root, "local", "data")); err != nil {
		t.Errorf("a directory outside the forbidden mount: %v", err)
	}
}

// A value shaped like a SQLite URI is a relative directory name: it resolves to
// an absolute path below the working directory, never to the URI's target.
func TestResolveDirIsFilesystemPath(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(base)
	got, err := ResolveDir("file:" + filepath.Join(base, "x"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(base, "file:"+filepath.Join(base, "x"))
	if got != want || !filepath.IsAbs(got) {
		t.Errorf("ResolveDir(file:...) = %q, want the absolute filesystem path %q", got, want)
	}
	if got := (Layout{Dir: got}).DB(); !filepath.IsAbs(got) || strings.HasPrefix(got, "file:") {
		t.Errorf("database path %q can be read as a SQLite URI", got)
	}
}

// The artwork path falls back to the standard library's JPEG where a wazero
// compiler is unavailable (arm/v7): the same codec round trip is run natively
// and as a test binary under QEMU for linux/arm64 and linux/arm/v7.
func TestJPEGRoundTrip(t *testing.T) {
	const w, h = 64, 48
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			src.Set(x, y, color.RGBA{R: uint8(x * 4), G: uint8(y * 5), B: uint8(x + y), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(buf.Bytes()))
	if err != nil || format != "jpeg" || cfg.Width != w || cfg.Height != h {
		t.Fatalf("DecodeConfig = %+v, %q, %v; want a %dx%d jpeg", cfg, format, err, w, h)
	}
	got, err := jpeg.Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	var sum, n int
	for y := range h {
		for x := range w {
			r1, g1, b1, _ := src.At(x, y).RGBA()
			r2, g2, b2, _ := got.At(x, y).RGBA()
			for _, d := range []int{int(r1>>8) - int(r2>>8), int(g1>>8) - int(g2>>8), int(b1>>8) - int(b2>>8)} {
				sum += max(d, -d)
				n++
			}
		}
	}
	if mean := float64(sum) / float64(n); mean > 6 {
		t.Fatalf("mean channel error after the round trip = %.2f, want <= 6", mean)
	}
	t.Log("JPEG-ROUNDTRIP ok")
}
