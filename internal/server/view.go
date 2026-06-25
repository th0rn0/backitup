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
	now := time.Now()
	var v dashboardView
	for _, c := range clients {
		latest, err := s.st.LatestRun(ctx, c.ID)
		if err != nil {
			return dashboardView{}, err
		}
		h := model.DeriveHealth(latest, time.Duration(c.ExpectedIntervalSecs)*time.Second, now)
		lastOffsite, err := s.st.LatestOffsite(ctx, c.ID)
		if err != nil {
			return dashboardView{}, err
		}
		row := clientRow{
			ID: c.ID, Name: c.Name, Slug: c.Slug(), Mode: string(c.Mode),
			Health: string(h), HealthLabel: healthLabel(h), Icon: healthIcon(h),
			Retention:        fmt.Sprintf("%dd", c.RetentionDays),
			Offsite:          offsiteLabel(c, lastOffsite),
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
