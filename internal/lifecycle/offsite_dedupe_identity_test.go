package lifecycle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/archiveutil"
	"github.com/th0rn0/backitup/internal/model"
	"golang.org/x/sys/unix"
)

// The offsite dedupe key is `fileChecksum` of the object the mode's
// PrepareOffsite produced (internal/lifecycle, offsiteSnapshot), i.e. sha256
// over the archive file. This file pins the two properties that keep a tree
// WITHOUT extended attributes out of the xattr feature's cost:
//
//   - the checksum the upload path records is exactly the digest of
//     archiveutil.TarGz over the snapshot directory — nothing is added at the
//     offsite layer, and no PAX record appears in the object — so the
//     byte-identity result in internal/archiveutil
//     (TestTarGzWithoutXattrsIsByteIdenticalToPreChangeWriter, against the
//     writer as it stood at cd5adf6) is the dedupe-checksum invariant;
//   - two snapshots of an unchanged unattributed tree still deduplicate, which
//     is that invariant as an operator sees it: an unchanged client does not
//     re-upload because a new writer release landed.
//
// And the guard that keeps the first from passing vacuously: a snapshot whose
// tree DOES carry `user.rsync.%stat` must not be recorded as a dedupe of the
// unattributed copy. If it were, the bucket would hold a byte-stream with no
// owner while the database claimed that snapshot was offsited.

// pinnedTree writes an rsync snapshot tree whose files and directories all have
// the same mtime. Tar records mtimes, so two snapshots of "the same" tree only
// have identical archives when the times are pinned — otherwise the second
// snapshot is newer for a reason that has nothing to do with its content.
func pinnedTree(t *testing.T, dir string) {
	t.Helper()
	pinned := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, rel := range []string{"etc/hosts", "etc/ssh/sshd_config"} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("content of "+rel), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, pinned, pinned); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("hosts", filepath.Join(dir, "etc", "hosts.link")); err != nil {
		t.Fatal(err)
	}
	// Deepest first, so a parent's mtime is not bumped after it was pinned.
	for _, rel := range []string{"etc/ssh", "etc"} {
		p := filepath.Join(dir, rel)
		if err := os.Chtimes(p, pinned, pinned); err != nil {
			t.Fatal(err)
		}
	}
}

// archiveDigest is sha256 over what PrepareOffsite builds: TarGz of the
// snapshot directory with the offsite worker's own arguments
// (internal/mode/rsync/server.go — nil excludes, symlinks kept).
func archiveDigest(t *testing.T, dir string) string {
	t.Helper()
	var buf bytes.Buffer
	if _, _, err := archiveutil.TarGz(context.Background(), &buf, dir, nil, false, nil); err != nil {
		t.Fatalf("archive %s: %v", dir, err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:])
}

// paxRecordNames lists the PAX records in a tar.gz object, by entry name. An
// empty result is how "this archive carries no extended attributes" is stated
// at the object level rather than by reading the writer.
func paxRecordNames(t *testing.T, gz []byte) map[string][]string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatalf("the uploaded object is not gzip: %v", err)
	}
	tr := tar.NewReader(zr)
	out := map[string][]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		if hdr.Typeflag == tar.TypeXHeader || hdr.Typeflag == tar.TypeXGlobalHeader {
			out["<format header>"] = append(out["<format header>"], hdr.Name)
			continue
		}
		for k := range hdr.PAXRecords {
			out[hdr.Name] = append(out[hdr.Name], k)
		}
	}
	return out
}

// setOwnershipXattr plants the attribute rsync's receiver-side --fake-super
// writes, skipping the test where the filesystem refuses user attributes.
func setOwnershipXattr(t *testing.T, path, value string) {
	t.Helper()
	if err := unix.Lsetxattr(path, "user.rsync.%stat", []byte(value), 0); err != nil {
		if err == unix.ENOTSUP || err == unix.EOPNOTSUPP || err == unix.EPERM {
			t.Skipf("this filesystem cannot store user xattrs: %v", err)
		}
		t.Fatalf("lsetxattr user.rsync.%%stat on %s: %v", path, err)
	}
}

// offsiteObjectOwner returns the owning (non-dedupe) row and the dedupe rows.
func offsiteObjectOwner(t *testing.T, rows []model.OffsiteObject) (model.OffsiteObject, []model.OffsiteObject) {
	t.Helper()
	var owner model.OffsiteObject
	var dedups []model.OffsiteObject
	for _, o := range rows {
		if o.DedupOf == "" {
			if owner.SnapshotID != "" {
				t.Fatalf("two owning offsite rows after one upload pass: %+v", rows)
			}
			owner = o
			continue
		}
		dedups = append(dedups, o)
	}
	return owner, dedups
}

// A tree without extended attributes still deduplicates: the checksum the
// offsite upload path records for it is the digest of the plain archive (so the
// byte-identity guarantee in internal/archiveutil is the dedupe guarantee), and
// a second snapshot of the same unchanged tree is recorded as a dedupe of the
// first rather than uploaded again.
func TestOffsiteDedupeChecksumIsUnaffectedForATreeWithoutXattrs(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	id, base, dir := newClient(t, st, model.Client{
		Name: "vmhost", Mode: model.ModeRsync, OffsiteRemote: "local",
		RetentionDays: 0, Enabled: true,
	})

	const older, newer = "20260601T000000Z", "20260618T000000Z"
	for _, sid := range []string{older, newer} {
		p := filepath.Join(dir, "snapshots", sid)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		pinnedTree(t, p)
	}
	if err := os.Symlink(newer, filepath.Join(dir, "snapshots", "latest")); err != nil {
		t.Fatal(err)
	}

	off := &fakeOffsite{}
	deps := Deps{Store: st, Offsite: off, BackupBaseDir: base, Now: func() time.Time { return now }}
	if err := RunOffsiteOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOffsiteOnce: %v", err)
	}

	// Unchanged content means one object and one dedupe record — through the
	// real PrepareOffsite, on a tree that carries no attributes at all.
	if len(off.up) != 1 {
		t.Fatalf("uploads = %d, want 1 (an unchanged tree is deduplicated): %v", len(off.up), off.up)
	}
	rows, err := st.ListOffsiteObjects(context.Background(), id)
	if err != nil {
		t.Fatalf("list offsite objects: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("offsite records = %d (%+v), want 2 (owner + dedupe)", len(rows), rows)
	}
	owner, dedups := offsiteObjectOwner(t, rows)
	if len(dedups) != 1 {
		t.Fatalf("dedupe rows = %d, want 1: %+v", len(dedups), rows)
	}
	if dedups[0].DedupOf != owner.SnapshotID {
		t.Fatalf("dedupe row points at %q, want the owner %q", dedups[0].DedupOf, owner.SnapshotID)
	}
	if dedups[0].Checksum != owner.Checksum {
		t.Fatalf("dedupe checksum %s, want the owner's %s", dedups[0].Checksum, owner.Checksum)
	}

	// The dedupe key is the archive digest and nothing else: the object the
	// uploader received hashes to the row's checksum, and that equals what
	// archiveutil.TarGz produces for the snapshot directory today.
	var obj string
	var body []byte
	for name, payload := range off.payload {
		obj, body = name, payload
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != owner.Checksum {
		t.Fatalf("object %s hashes to %s, but the offsite row records %s", obj, got, owner.Checksum)
	}
	if want := archiveDigest(t, filepath.Join(dir, "snapshots", newer)); owner.Checksum != want {
		t.Fatalf("recorded dedupe checksum %s, want the plain archive digest %s", owner.Checksum, want)
	}
	if owner.Bytes != int64(len(body)) {
		t.Fatalf("row bytes = %d, want the object size %d", owner.Bytes, len(body))
	}

	// And no PAX record reached the object: nothing about this tree changed.
	if pax := paxRecordNames(t, body); len(pax) != 0 {
		t.Fatalf("the object of a tree without xattrs carries PAX records %v; the archive shape (and therefore the dedupe checksum) changed", pax)
	}
}

// The guard for the identity test above: an archive of a tree that DOES carry
// `user.rsync.%stat` must not dedupe against the unattributed copy of the same
// content. If it did, "offsited" would be recorded for a snapshot whose object
// has no owner — the B2 boundary losing exactly what the hot store kept.
func TestOffsiteArchiveOfAnAttributedTreeDoesNotDedupeAgainstThePlainOne(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	id, base, dir := newClient(t, st, model.Client{
		Name: "vmhost", Mode: model.ModeRsync, OffsiteRemote: "local",
		RetentionDays: 0, Enabled: true,
	})

	const plain, owned = "20260601T000000Z", "20260618T000000Z"
	for _, sid := range []string{plain, owned} {
		p := filepath.Join(dir, "snapshots", sid)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		pinnedTree(t, p)
	}
	// Same bytes in both trees; the only difference is the ownership record.
	// 1000:1000 is deliberately not this process's uid/gid.
	const pseudostat = "100644 0,0 1000:1000"
	setOwnershipXattr(t, filepath.Join(dir, "snapshots", owned, "etc", "hosts"), pseudostat)
	if err := os.Symlink(owned, filepath.Join(dir, "snapshots", "latest")); err != nil {
		t.Fatal(err)
	}

	off := &fakeOffsite{}
	deps := Deps{Store: st, Offsite: off, BackupBaseDir: base, Now: func() time.Time { return now }}
	if err := RunOffsiteOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOffsiteOnce: %v", err)
	}

	// Both trees go up: the attribute is content, so the checksums differ.
	if len(off.up) != 2 {
		t.Fatalf("uploads = %d, want 2 (an attributed tree is not the same object as the plain one): %v", len(off.up), off.up)
	}
	rows, err := st.ListOffsiteObjects(context.Background(), id)
	if err != nil {
		t.Fatalf("list offsite objects: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("offsite records = %d (%+v), want 2 (two objects, no dedupe)", len(rows), rows)
	}
	byID := map[string]model.OffsiteObject{}
	for _, o := range rows {
		if o.DedupOf != "" {
			t.Fatalf("snapshot %s was recorded as a dedupe of %s: an archive without the owner was treated as identical to one with it", o.SnapshotID, o.DedupOf)
		}
		byID[o.SnapshotID] = o
	}
	if byID[plain].Checksum == byID[owned].Checksum {
		t.Fatalf("both trees hashed to %s: the attribute is not reaching the archive", byID[plain].Checksum)
	}

	// And the difference is the attribute itself, on the entry that carries it.
	for name, payload := range off.payload {
		if bytes.Contains([]byte(name), []byte(owned)) {
			pax := paxRecordNames(t, payload)
			var got bool
			for _, keys := range pax {
				for _, k := range keys {
					if k == "SCHILY.xattr.user.rsync.%stat" {
						got = true
					}
				}
			}
			if !got {
				t.Fatalf("the object for %s carries no SCHILY.xattr.user.rsync.%%stat record: %v", owned, pax)
			}
		}
	}
}
