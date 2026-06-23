package client

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/th0rn0/backitup/internal/mode"
	"github.com/th0rn0/backitup/internal/model"
)

// Run executes one backup: take the per-client lock, fetch config over the
// control channel, run the mode's backup, and report status back. A held lock
// is reported as "overlap" and is not an error (fire-and-forget; design doc).
func Run(ctx context.Context, cfg Config, lockPath string) error {
	api, err := NewAPI(cfg.APIBase, cfg.Token, cfg.CABundle, cfg.Insecure)
	if err != nil {
		return err
	}

	lk, held, err := Acquire(lockPath)
	if err != nil {
		return err
	}
	if held {
		log.Printf("backitup: previous run still in progress, skipping (overlap)")
		// Best-effort: tell the server we skipped, so the dashboard is truthful.
		now := time.Now().UTC()
		_ = api.PostStatus(ctx, StatusReq{Status: string(model.StatusOverlap), StartedAt: now, FinishedAt: now})
		return nil
	}
	defer lk.Release()

	// Server owns WHAT (D1): config is authoritative; the flag mode is a fallback.
	scfg, err := api.FetchConfig(ctx)
	if err != nil {
		return err
	}
	m := model.Mode(scfg.Mode)
	if !m.Valid() {
		m = cfg.Mode
	}
	cm, ok := mode.Client(m)
	if !ok {
		return fmt.Errorf("mode %q not supported by this client", m)
	}

	res, backupErr := cm.Backup(ctx, mode.BackupOpts{
		SourceDir:    cfg.Source,
		Excludes:     scfg.Excludes,
		SkipSymlinks: scfg.SkipSymlinks || cfg.SkipSymlinks,
		SSHServer:    cfg.SSHServer,
		SSHUser:      cfg.SSHUser,
		SSHKey:       cfg.SSHKey,
		KnownHosts:   cfg.KnownHosts,
		Insecure:     cfg.Insecure,
	})

	sreq := buildStatus(res, backupErr)
	if perr := api.PostStatus(ctx, sreq); perr != nil {
		log.Printf("backitup: failed to report status: %v", perr)
		if backupErr == nil {
			return perr
		}
	}
	return backupErr
}

func buildStatus(res mode.BackupResult, backupErr error) StatusReq {
	s := StatusReq{
		Status:     string(model.StatusOK),
		Bytes:      res.Bytes,
		Files:      res.Files,
		SnapshotID: res.SnapshotID,
		StartedAt:  res.StartedAt,
		FinishedAt: res.FinishedAt,
	}
	if backupErr != nil {
		s.Status = string(model.StatusFailed)
		s.LogTail = model.CapLogTail(backupErr.Error())
	}
	if s.FinishedAt.IsZero() {
		now := time.Now().UTC()
		if s.StartedAt.IsZero() {
			s.StartedAt = now
		}
		s.FinishedAt = now
	}
	return s
}
