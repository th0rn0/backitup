// Package archiveutil builds gzip+tar streams of a directory. Shared by the
// tar.gz client mode (stream to the server) and rsync offsite prep (tar a
// snapshot dir into one immutable object, resolving hardlinks).
package archiveutil

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// TarGz writes a gzip-compressed tar of srcDir (contents, not the root dir) to w,
// returning the regular-file count and total uncompressed bytes. Entries matching
// an exclude glob (by full relative path or base name) are skipped. srcDir is
// only ever READ.
func TarGz(ctx context.Context, w io.Writer, srcDir string, excludes []string) (files, written int64, err error) {
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
		// Skip file types that tar cannot represent: sockets, devices, FIFOs.
		if m := info.Mode(); m&(os.ModeSocket|os.ModeDevice|os.ModeNamedPipe|os.ModeIrregular) != 0 {
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

// VerifyGzip reads a .gz file fully, surfacing gzip header/CRC errors — a cheap
// "is this archive actually intact" integrity check (D9).
func VerifyGzip(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip header: %w", err)
	}
	defer gr.Close()
	if _, err := io.Copy(io.Discard, gr); err != nil {
		return fmt.Errorf("gzip stream: %w", err)
	}
	return nil
}

// Excluded reports whether rel matches any glob, by full relative path or base name.
func Excluded(rel string, patterns []string) bool {
	base := path.Base(filepath.ToSlash(rel))
	for _, pat := range patterns {
		pat = trimSlash(pat)
		if ok, _ := path.Match(pat, filepath.ToSlash(rel)); ok {
			return true
		}
		if ok, _ := path.Match(pat, base); ok {
			return true
		}
	}
	return false
}

func trimSlash(s string) string {
	for len(s) > 1 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
