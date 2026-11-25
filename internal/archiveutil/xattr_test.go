package archiveutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// tarEntries returns every entry of a gzipped tar stream, keyed by name.
func tarEntries(t *testing.T, r io.Reader) map[string]*tar.Header {
	t.Helper()
	gr, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gr)
	out := map[string]*tar.Header{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		out[hdr.Name] = hdr
	}
}

// setUserXattr writes one attribute, skipping the test when the temp directory
// cannot hold one (a container on a filesystem that refuses user xattrs).
func setUserXattr(t *testing.T, path, name, value string) {
	t.Helper()
	if err := unix.Lsetxattr(path, name, []byte(value), 0); err != nil {
		if err == unix.ENOTSUP || err == unix.EOPNOTSUPP || err == unix.EPERM {
			t.Skipf("this filesystem cannot store user xattrs: %v", err)
		}
		t.Fatalf("lsetxattr %s on %s: %v", name, path, err)
	}
}

// The whole reason this exists: an rsync snapshot records the source's owner in
// `user.rsync.%stat`, so an offsite archive that dropped the attribute would keep
// the owner in the hot tier and lose it the moment the snapshot is uploaded.
func TestTarGzCarriesUserXattrs(t *testing.T) {
	src := t.TempDir()
	file := filepath.Join(src, "owned.txt")
	write(t, file, "hello")
	// The exact value rsync --fake-super writes: mode, rdev, then uid:gid.
	const pseudostat = "100644 0,0 1000:1000"
	setUserXattr(t, file, "user.rsync.%stat", pseudostat)
	setUserXattr(t, file, "user.note", "kept too")

	var buf bytes.Buffer
	if _, _, err := TarGz(context.Background(), &buf, src, nil, false, nil); err != nil {
		t.Fatalf("TarGz: %v", err)
	}
	hdr := tarEntries(t, &buf)["owned.txt"]
	if hdr == nil {
		t.Fatal("owned.txt missing from the archive")
	}
	if got := hdr.PAXRecords["SCHILY.xattr.user.rsync.%stat"]; got != pseudostat {
		t.Fatalf("SCHILY.xattr.user.rsync.%%stat = %q, want %q (PAX records: %v)", got, pseudostat, hdr.PAXRecords)
	}
	if got := hdr.PAXRecords["SCHILY.xattr.user.note"]; got != "kept too" {
		t.Fatalf("SCHILY.xattr.user.note = %q, want %q", got, "kept too")
	}
}

// And back out of it: extracting the archive with `tar --xattrs` (which reads
// SCHILY.xattr records, the GNU spelling) must leave the attribute on the file.
func TestTarGzXattrRoundTrip(t *testing.T) {
	src := t.TempDir()
	file := filepath.Join(src, "owned.txt")
	write(t, file, "hello")
	const pseudostat = "100644 0,0 1000:1000"
	setUserXattr(t, file, "user.rsync.%stat", pseudostat)

	var buf bytes.Buffer
	if _, _, err := TarGz(context.Background(), &buf, src, nil, false, nil); err != nil {
		t.Fatalf("TarGz: %v", err)
	}
	// Copy the bytes before reading: tarEntries drains the buffer.
	raw := append([]byte(nil), buf.Bytes()...)
	hdr := tarEntries(t, bytes.NewReader(raw))["owned.txt"]
	if _, ok := hdr.PAXRecords["SCHILY.xattr.user.rsync.%stat"]; !ok {
		t.Fatalf("archive carries no user.rsync.%%stat (PAX records: %v)", hdr.PAXRecords)
	}

	dst := t.TempDir()
	archive := filepath.Join(dst, "snap.tar.gz")
	if err := os.WriteFile(archive, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// Extract with the command the runbook and the client page document, not by
	// re-setting the attribute from the PAX value in Go: the claim is that GNU
	// tar reads these records and writes the attribute back, and only tar can
	// prove that.
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("GNU tar is not on PATH: the documented extract command cannot be exercised")
	}
	out := filepath.Join(dst, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("tar", "--xattrs", "--xattrs-include=*", "-xzpf", archive, "-C", out)
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tar --xattrs extract: %v: %s", err, msg)
	}
	restored := filepath.Join(out, "owned.txt")
	xbuf := make([]byte, 256)
	n, err := unix.Lgetxattr(restored, "user.rsync.%stat", xbuf)
	if err != nil {
		t.Fatalf("reading the extracted attribute: %v", err)
	}
	if string(xbuf[:n]) != pseudostat {
		t.Fatalf("extracted attribute = %q, want %q", string(xbuf[:n]), pseudostat)
	}
}

// A tree with no attributes must produce no PAX header at all: with the
// ownership setting off, an offsite archive is byte-for-byte what it was before
// this change.
func TestTarGzWithoutXattrsHasNoPAXRecords(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "plain.txt"), "hello")

	var buf bytes.Buffer
	if _, _, err := TarGz(context.Background(), &buf, src, nil, false, nil); err != nil {
		t.Fatalf("TarGz: %v", err)
	}
	// Copy the bytes before reading: tarEntries drains the buffer, and
	// bytes.Buffer.Bytes() is the *unread* remainder.
	raw := append([]byte(nil), buf.Bytes()...)
	hdr := tarEntries(t, bytes.NewReader(raw))["plain.txt"]
	if hdr == nil {
		t.Fatal("plain.txt missing from the archive")
	}
	if len(hdr.PAXRecords) != 0 {
		t.Fatalf("plain entry carries PAX records %v, want none", hdr.PAXRecords)
	}
	// A header carrying PAX records is emitted as an extra extended-header entry
	// before the real one, so "no records" also means exactly one entry.
	if n := len(tarEntries(t, bytes.NewReader(raw))); n != 1 {
		t.Fatalf("archive has %d entries, want 1 (a PAX header would add one)", n)
	}
}

// Non-user namespaces are not carried: the app runs unprivileged, cannot read
// them, and could not restore them anyway.
func TestReadUserXattrsIgnoresOtherNamespaces(t *testing.T) {
	src := t.TempDir()
	file := filepath.Join(src, "f")
	write(t, file, "x")
	setUserXattr(t, file, "user.kept", "1")

	got := readUserXattrs(file)
	if len(got) != 1 || got[0].name != "user.kept" {
		t.Fatalf("readUserXattrs = %v, want only user.kept", got)
	}
	if strings.Contains(got[0].name, "system.") {
		t.Fatalf("a non-user namespace leaked: %v", got)
	}
}
