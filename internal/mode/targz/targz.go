// Package targz implements the tar.gz backup mode (design doc Approach A).
//
// Client side: stream a gzip-compressed tar of the source over SSH to the
// server's forced command (backitup-recv), which writes it into this client's
// confined directory. Pure Go (archive via internal/archiveutil + x/crypto/ssh),
// no external tools. The source is only ever READ.
//
// Server side (Lane D): see server.go — list/prune archives, offsite each one.
package targz

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/th0rn0/backitup/internal/archiveutil"
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
	logger := o.Log()
	start := time.Now().UTC()

	logger.Printf("connecting to %s", o.SSHServer)
	conn, err := dial(o)
	if err != nil {
		return mode.BackupResult{}, err
	}
	defer conn.Close() //nolint:errcheck
	sess, err := conn.NewSession()
	if err != nil {
		return mode.BackupResult{}, fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close() //nolint:errcheck

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

	logger.Printf("archiving %s", o.SourceDir)
	progress := func(files, bytes int64) {
		logger.Printf("archiving... %d files, %s", files, mode.HumanBytes(bytes))
	}
	files, written, archiveErr := archiveutil.TarGz(ctx, stdin, o.SourceDir, o.Excludes, o.SkipSymlinks, progress)
	// Always close stdin so the remote sees EOF, even on error.
	_ = stdin.Close()
	waitErr := sess.Wait()

	if archiveErr != nil {
		return mode.BackupResult{}, archiveErr
	}
	if waitErr != nil {
		return mode.BackupResult{}, fmt.Errorf("remote upload failed: %w: %s", waitErr, remoteErr.String())
	}
	logger.Printf("archived %d files, %s", files, mode.HumanBytes(written))
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
