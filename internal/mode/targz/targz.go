// Package targz implements the tar.gz client backup mode (design doc Approach A).
// It streams a gzip-compressed tar of the source directory over SSH to the
// server's forced command (backitup-recv), which writes it into this client's
// confined directory. Pure Go: archive/tar + x/crypto/ssh, no external tools.
//
// The source is only ever READ (non-destructiveness invariant).
package targz

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/th0rn0/backitup/internal/mode"
	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/sshutil"
)

// Mode is the tar.gz client mode.
type Mode struct{}

func (Mode) Mode() model.Mode { return model.ModeTarGz }

func init() { mode.RegisterClient(Mode{}) }

// Backup streams a tar.gz of o.SourceDir to the server over SSH.
func (Mode) Backup(ctx context.Context, o mode.BackupOpts) (mode.BackupResult, error) {
	start := time.Now().UTC()

	conn, err := dial(o)
	if err != nil {
		return mode.BackupResult{}, err
	}
	defer conn.Close()

	sess, err := conn.NewSession()
	if err != nil {
		return mode.BackupResult{}, fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()

	stdin, err := sess.StdinPipe()
	if err != nil {
		return mode.BackupResult{}, fmt.Errorf("stdin pipe: %w", err)
	}
	var remoteErr bytes.Buffer
	sess.Stderr = &remoteErr

	// The requested command is ignored; sshd runs the forced backitup-recv.
	if err := sess.Start("backup"); err != nil {
		return mode.BackupResult{}, fmt.Errorf("start remote: %w", err)
	}

	files, written, archiveErr := streamArchive(ctx, stdin, o)
	// Always close the writers so the remote sees EOF, even on error.
	_ = stdin.Close()
	waitErr := sess.Wait()

	if archiveErr != nil {
		return mode.BackupResult{}, archiveErr
	}
	if waitErr != nil {
		return mode.BackupResult{}, fmt.Errorf("remote upload failed: %w: %s", waitErr, remoteErr.String())
	}
	return mode.BackupResult{
		Bytes:      written,
		Files:      files,
		StartedAt:  start,
		FinishedAt: time.Now().UTC(),
	}, nil
}

func dial(o mode.BackupOpts) (*ssh.Client, error) {
	signer, err := sshutil.LoadSigner(o.SSHKey)
	if err != nil {
		return nil, err
	}
	cb, err := sshutil.HostKeyCallback(o.KnownHosts, o.Insecure)
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User:            o.SSHUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: cb,
		Timeout:         30 * time.Second,
	}
	conn, err := ssh.Dial("tcp", o.SSHServer, cfg)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", o.SSHServer, err)
	}
	return conn, nil
}

// streamArchive writes a gzip+tar of o.SourceDir to w, returning the file count
// and total uncompressed bytes. Reads only; never writes the source.
func streamArchive(ctx context.Context, w io.Writer, o mode.BackupOpts) (files, written int64, err error) {
	gw := gzip.NewWriter(w)
	tw := tar.NewWriter(gw)

	walkErr := filepath.WalkDir(o.SourceDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, err := filepath.Rel(o.SourceDir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil // don't archive the root itself
		}
		if excluded(rel, o.Excludes) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
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
	// Close tar then gzip so the stream is well-formed; surface the first error.
	twErr := tw.Close()
	gwErr := gw.Close()
	switch {
	case walkErr != nil:
		return files, written, fmt.Errorf("archive source: %w", walkErr)
	case twErr != nil:
		return files, written, fmt.Errorf("close tar: %w", twErr)
	case gwErr != nil:
		return files, written, fmt.Errorf("close gzip: %w", gwErr)
	}
	return files, written, nil
}

// excluded reports whether rel matches any exclude glob, by full relative path
// or by base name (so "node_modules" or "*.tmp" both work).
func excluded(rel string, patterns []string) bool {
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
