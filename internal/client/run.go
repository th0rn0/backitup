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
	rl := NewRunLogger(cfg.Quiet)
	api, err := NewAPI(cfg.APIBase, cfg.Token, cfg.CABundle, cfg.InsecureTLS)
	if err != nil {
		return err
	}

	lk, held, err := Acquire(lockPath)
	if err != nil {
		return err
	}
	if held {
		rl.Printf("backup: previous run still in progress, skipping (overlap)")
		now := time.Now().UTC()
		sreq := StatusReq{Status: string(model.StatusOverlap), StartedAt: now, FinishedAt: now, LogTail: rl.String()}
		_, _ = api.PostStatus(ctx, sreq)
		return nil
	}
	defer lk.Release()

	// Server owns WHAT (D1): config is authoritative; the flag mode is a fallback.
	scfg, err := api.FetchConfig(ctx)
	if err != nil {
		return err
	}
	rl.Printf("config: mode=%s excludes=%v skip_symlinks=%v", scfg.Mode, scfg.Excludes, scfg.SkipSymlinks || cfg.SkipSymlinks)
	m := model.Mode(scfg.Mode)
	if !m.Valid() {
		m = cfg.Mode
	}
	cm, ok := mode.Client(m)
	if !ok {
		return fmt.Errorf("mode %q not supported by this client", m)
	}

	// Signal that this client is actively running so the dashboard shows "running".
	now := time.Now().UTC()
	runID, _ := api.PostStatus(ctx, StatusReq{Status: string(model.StatusRunning), StartedAt: now})

	res, backupErr := cm.Backup(ctx, mode.BackupOpts{
		SourceDir:           cfg.Source,
		Excludes:            scfg.Excludes,
		SkipSymlinks:        scfg.SkipSymlinks || cfg.SkipSymlinks,
		HasPreviousSnapshot: scfg.HasPreviousSnapshot,
		Logger:              rl.Logger,
		SSHServer:           cfg.SSHServer,
		SSHUser:             cfg.SSHUser,
		SSHKey:              cfg.SSHKey,
		KnownHosts:          cfg.KnownHosts,
		InsecureSSH:         cfg.InsecureSSH,
	})
	if backupErr != nil {
		rl.Printf("backup error: %v", backupErr)
	}

	sreq := buildStatus(res, backupErr)
	sreq.LogTail = model.CapLogTail(rl.String())
	sreq.RunID = runID
	// Use a fresh context: the run context may be cancelled (e.g. SIGTERM killed
	// rsync mid-transfer) but we still need to update the "running" row to failed.
	finalCtx, finalCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer finalCancel()
	if _, perr := api.PostStatus(finalCtx, sreq); perr != nil {
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
