package server

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

type dashboardView struct {
	Username   string
	ActivePage string
	Summary    summaryCounts
	Clients    []clientRow
	Storage    storageStats
}

type storageStats struct {
	HotBytes  int64
	ColdBytes int64
}

type summaryCounts struct{ OK, Stale, Failed, Never, Running int }

type clientRow struct {
	ID              int64
	Name            string
	Slug            string // URL/filesystem-safe identifier derived from Name
	Mode            string
	Health          string // css class: ok/stale/failed/never
	HealthLabel     string // "OK" / "Stale" / "Failed" / "Never"
	Icon            string // ● / ▲ / ✖ / ○  (icon+colour+text; never colour alone, DD2)
	LastBackup      string
	Size            string
	Retention       string
	Offsite         string
	OffsiteUploading bool // true while an adhoc offsite run is in progress
}

// buildDashboard loads the fleet and shapes it for the template: per-client
// health, human-friendly fields, summary counts, and failed/stale-first order.
func (s *Server) buildDashboard(ctx context.Context) (dashboardView, error) {
	clients, err := s.st.ListClients(ctx)
	if err != nil {
		return dashboardView{}, err
	}
	uploading, err := s.st.RunningOffsiteClientIDs(ctx)
	if err != nil {
		return dashboardView{}, err
	}
	latestRuns, err := s.st.LatestRunAllClients(ctx)
	if err != nil {
		return dashboardView{}, err
	}
	latestOffsites, err := s.st.LatestOffsiteAllClients(ctx)
	if err != nil {
		return dashboardView{}, err
	}
	now := time.Now()
	var v dashboardView
	for _, c := range clients {
		latest := latestRuns[c.ID]
		h := model.DeriveHealth(latest, time.Duration(c.ExpectedIntervalSecs)*time.Second, now)
		row := clientRow{
			ID: c.ID, Name: c.Name, Slug: c.Slug(), Mode: string(c.Mode),
			Health: string(h), HealthLabel: healthLabel(h), Icon: healthIcon(h),
			Retention:        fmt.Sprintf("%dd", c.RetentionDays),
			Offsite:          offsiteLabel(c, latestOffsites[c.ID]),
			OffsiteUploading: uploading[c.ID],
		}
		switch {
		case latest == nil:
			row.LastBackup, row.Size = "never", "—"
		case latest.Status == model.StatusRunning:
			row.LastBackup, row.Size = "in progress", "…"
		case latest.Status == model.StatusFailed:
			row.LastBackup, row.Size = "FAILED "+relTime(now.Sub(latest.FinishedAt)), "—"
		default:
			row.LastBackup, row.Size = relTime(now.Sub(latest.FinishedAt)), humanBytes(latest.Bytes)
		}
		switch h {
		case model.HealthOK:
			v.Summary.OK++
		case model.HealthStale:
			v.Summary.Stale++
		case model.HealthFailed:
			v.Summary.Failed++
		case model.HealthNever:
			v.Summary.Never++
		case model.HealthRunning:
			v.Summary.Running++
		}
		v.Clients = append(v.Clients, row)
	}
	// DD1: problems sort to the top (failed, then stale, then never, then ok),
	// stable on name within a group.
	sort.SliceStable(v.Clients, func(i, j int) bool {
		return healthRank(v.Clients[i].Health) < healthRank(v.Clients[j].Health)
	})

	// Storage totals: hot bytes are cached (60s TTL) to avoid walking the
	// backup dir tree on every page load.
	v.Storage.ColdBytes, _ = s.st.TotalOffsiteBytes(ctx)
	v.Storage.HotBytes = s.cachedHotBytes()

	return v, nil
}

// cachedHotBytes returns the true on-disk size of the backup dir, recomputing
// at most once per minute. Hardlinked files (rsync snapshots share inodes) are
// counted once regardless of how many snapshots reference them.
func (s *Server) cachedHotBytes() int64 {
	s.hotBytesMu.Lock()
	defer s.hotBytesMu.Unlock()
	if time.Now().Before(s.hotBytesExpiry) {
		return s.hotBytesCache
	}
	type inodeKey struct{ dev, ino uint64 }
	seen := make(map[inodeKey]struct{})
	var total int64
	_ = filepath.WalkDir(s.backupBaseDir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if sys, ok := info.Sys().(*syscall.Stat_t); ok {
			key := inodeKey{sys.Dev, sys.Ino}
			if _, dup := seen[key]; dup {
				return nil
			}
			seen[key] = struct{}{}
		}
		total += info.Size()
		return nil
	})
	s.hotBytesCache = total
	s.hotBytesExpiry = time.Now().Add(60 * time.Second)
	return total
}

func healthRank(h string) int {
	switch model.Health(h) {
	case model.HealthFailed:
		return 0
	case model.HealthStale:
		return 1
	case model.HealthNever:
		return 2
	case model.HealthRunning:
		return 3
	default: // ok
		return 4
	}
}

func healthLabel(h model.Health) string {
	switch h {
	case model.HealthOK:
		return "OK"
	case model.HealthStale:
		return "Stale"
	case model.HealthFailed:
		return "Failed"
	case model.HealthRunning:
		return "Running"
	default:
		return "Never"
	}
}

func healthIcon(h model.Health) string {
	switch h {
	case model.HealthOK:
		return "●"
	case model.HealthStale:
		return "▲"
	case model.HealthFailed:
		return "✖"
	case model.HealthRunning:
		return "◎"
	default:
		return "○"
	}
}

// offsiteLabel reflects real offsite state: not configured (—), configured but
// nothing tiered yet (⚠ pending), or last upload time + remote (✓).
func offsiteLabel(c model.Client, lastOffsite *time.Time) string {
	if c.OffsiteRemote == "" {
		return "—"
	}
	if lastOffsite == nil {
		return "⚠ " + c.OffsiteRemote + " pending"
	}
	return "✓ " + c.OffsiteRemote + " " + relTime(time.Since(*lastOffsite))
}

func relTime(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
