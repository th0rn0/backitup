package archiveutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// preXattrTarGz is TarGz exactly as it stood at cd5adf6 — before extended
// attributes were carried as PAX records. It is kept here, verbatim, as the
// reference for "an archive of a tree without xattrs is unchanged": the offsite
// object for such a tree must stay byte-identical, because its sha256 is the
// dedupe key (`fileChecksum` in internal/lifecycle hashes the archive) and a
// changed byte would re-upload every client's unchanged tree.
//
// It is a verbatim copy on purpose. Extracting a shared implementation would
// make the comparison tautological and let a change to TarGz slip through in
// both.
func preXattrTarGz(ctx context.Context, w io.Writer, srcDir string, excludes []string, skipSymlinks bool, progress ProgressFunc) (files, written int64, err error) {
	gw := gzip.NewWriter(w)
	tw := tar.NewWriter(gw)

	walkErr := filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if Excluded(rel, excludes) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		m := info.Mode()
		if m&(os.ModeSocket|os.ModeDevice|os.ModeNamedPipe|os.ModeIrregular) != 0 {
			return nil
		}
		if skipSymlinks && m&os.ModeSymlink != 0 {
			return nil
		}
		var link string
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			n, cpErr := io.Copy(tw, f)
			f.Close()
			if cpErr != nil {
				return cpErr
			}
			files++
			written += n
			if progress != nil && files%1000 == 0 {
				progress(files, written)
			}
		}
		return nil
	})

	twErr := tw.Close()
	gwErr := gw.Close()
	switch {
	case walkErr != nil:
		return files, written, fmt.Errorf("archive %s: %w", srcDir, walkErr)
	case twErr != nil:
		return files, written, fmt.Errorf("close tar: %w", twErr)
	case gwErr != nil:
		return files, written, fmt.Errorf("close gzip: %w", gwErr)
	}
	return files, written, nil
}

// archive builds the archive of src with the given writer and returns the
// container's path (TarGz writes to an io.Writer, and the offsite tier writes to
// a file whose sha256 becomes the dedupe key).
func archiveToFile(t *testing.T, src string, write func(context.Context, io.Writer, string, []string, bool, ProgressFunc) (int64, int64, error)) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "snapshot.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := write(context.Background(), f, src, nil, false, nil); err != nil {
		f.Close()
		t.Fatalf("archive %s: %v", src, err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// shapeTree is a tree with no extended attributes: two directories, two files
// (one empty), and a symlink.
func shapeTree(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "photos", "raw"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "top.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "photos", "raw", "shot.cr2"), []byte("payload"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "photos", "empty"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../top.txt", filepath.Join(src, "photos", "link.txt")); err != nil {
		t.Fatal(err)
	}
	return src
}

// The offsite object of a tree with no extended attributes is byte-for-byte the
// object cd5adf6 produced: same entries, same headers, same size, and therefore
// the same dedupe checksum. A tree that carries no attribute must not pay for
// the feature.
func TestTarGzWithoutXattrsIsByteIdenticalToPreChangeWriter(t *testing.T) {
	src := shapeTree(t)

	now := archiveToFile(t, src, TarGz)
	before := archiveToFile(t, src, preXattrTarGz)

	got, err := os.ReadFile(now)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(before)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("archive size %d bytes, want the pre-change %d (an archive with PAX records adds entries)", len(got), len(want))
	}
	if !bytes.Equal(got, want) {
		i := 0
		for i < len(got) && i < len(want) && got[i] == want[i] {
			i++
		}
		t.Fatalf("archive differs from the pre-change writer at byte %d of %d; the offsite dedupe checksum would change for an unchanged tree", i, len(got))
	}

	// Stated separately because the dedupe key is a checksum of this file, not
	// its size: `fileChecksum` (internal/lifecycle) is sha256 over the archive.
	sumNow := fmt.Sprintf("%x", sha256.Sum256(got))
	sumBefore := fmt.Sprintf("%x", sha256.Sum256(want))
	if sumNow != sumBefore {
		t.Fatalf("dedupe checksum = %s, want the pre-change %s", sumNow, sumBefore)
	}
}

// The comparison above only means something if the two writers can differ, so
// the same tree with a fake-super attribute must produce a different archive.
// Without this, a TarGz that silently stopped carrying attributes would still
// pass the byte-identity test.
func TestTarGzWithXattrDiffersFromPreChangeWriter(t *testing.T) {
	src := shapeTree(t)
	setUserXattr(t, filepath.Join(src, "top.txt"), "user.rsync.%stat", "100644 0,0 1000:1000")

	now, err := os.ReadFile(archiveToFile(t, src, TarGz))
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(archiveToFile(t, src, preXattrTarGz))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(now, before) {
		t.Fatal("the archive is unchanged by an entry that carries an xattr: the attribute is not being written")
	}

	// And the difference is the attribute itself, on the entry that has it.
	hdr := tarEntries(t, bytes.NewReader(now))["top.txt"]
	if hdr == nil {
		t.Fatal("top.txt missing from the archive")
	}
	if got := hdr.PAXRecords["SCHILY.xattr.user.rsync.%stat"]; got != "100644 0,0 1000:1000" {
		t.Fatalf("SCHILY.xattr.user.rsync.%%stat = %q, want the source value", got)
	}
}
