package server

import (
	"context"
	"fmt"
	"sort"
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
	HotBytes    int64
	HotMeasured bool   // at least one enabled client has a stored measurement
	HotPartial  bool   // the total is a lower bound: a capped walk, or a client not measured yet
	HotAsOf     string // age of the OLDEST measurement included in HotBytes ("" when none)
	HotMissing  int    // enabled clients with no measurement yet — omitted from HotBytes
	ColdBytes   int64
}

type summaryCounts struct{ OK, Stale, Failed, Never, Running int }

func (s summaryCounts) Total() int { return s.OK + s.Stale + s.Failed + s.Never + s.Running }

type clientRow struct {
	ID               int64
	Name             string
	Slug             string // URL/filesystem-safe identifier derived from Name
	Mode             string
	Health           string // css class: ok/stale/failed/never
	HealthLabel      string // "OK" / "Stale" / "Failed" / "Never"
	Icon             string // ● / ▲ / ✖ / ○  (icon+colour+text; never colour alone, DD2)
	LastBackup       string
	Size             string
	Retention        string
	Offsite          string
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
	lastOKRuns, err := s.st.LastSuccessfulRunAllClients(ctx)
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
		h := model.DeriveHealth(latest, lastOKRuns[c.ID], time.Duration(c.ExpectedIntervalSecs)*time.Second, now)
		row := clientRow{
			ID: c.ID, Name: c.Name, Slug: c.Slug, Mode: string(c.Mode),
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
		case latest.Status == model.StatusOverlap:
			// An overlap is not a backup: name it instead of presenting the
			// newest trigger as if it had captured anything (S8).
			row.LastBackup, row.Size = "overlap "+relTime(now.Sub(latest.FinishedAt)), "—"
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

	// Storage totals. Hot bytes are the lifecycle's per-client measurements
	// summed here: this handler must never walk the backup tree (see
	// lifecycle.measureClientStorage), and the age shown is the OLDEST
	// measurement included so the figure never flatters itself. A client with no
	// measurement is counted, not assumed to be zero, and makes the total a
	// lower bound.
	measured, err := s.st.ListClientStorage(ctx)
	if err != nil {
		return dashboardView{}, err
	}
	var oldest time.Time
	for _, c := range clients {
		if !c.Enabled {
			continue
		}
		st, ok := measured[c.ID]
		if !ok || st.MeasuredAt.IsZero() {
			v.Storage.HotMissing++
			continue
		}
		v.Storage.HotBytes += st.Bytes
		v.Storage.HotMeasured = true
		if st.Partial {
			v.Storage.HotPartial = true
		}
		if oldest.IsZero() || st.MeasuredAt.Before(oldest) {
			oldest = st.MeasuredAt
		}
	}
	if v.Storage.HotMissing > 0 {
		v.Storage.HotPartial = true
	}
	if !oldest.IsZero() {
		v.Storage.HotAsOf = relTime(now.Sub(oldest))
	}
	v.Storage.ColdBytes, _ = s.st.TotalOffsiteBytes(ctx)

	return v, nil
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
