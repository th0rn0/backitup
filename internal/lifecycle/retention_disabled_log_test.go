package lifecycle

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

// A retention horizon of 0 turns pruning off, and before this the pass returned
// in silence: from the outside "retention is 0" looked identical to "nothing is
// old enough yet", which is the wrong answer to give an operator watching cold
// storage grow. One line per pass per client, naming the setting responsible.
func TestRunOnceReportsDisabledOffsiteRetention(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	_, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeRsync, OffsiteRemote: "local",
		RetentionDays: 14, OffsiteRetentionDays: 0, Enabled: true,
	})
	mkRsyncSnapshots(t, dir, "20260618T100000Z", "20260618T110000Z")

	logs := runOnceCapturingLog(t, Deps{
		Store: st, Offsite: &fakeOffsite{}, BackupBaseDir: base,
		Now: func() time.Time { return now },
	})

	want := `offsite retention disabled for client "c" (offsite_retention_days=0): no objects will be pruned`
	if !strings.Contains(logs, want) {
		t.Fatalf("a pass with offsite_retention_days=0 must say why nothing is pruned;\nwant: %s\ngot:\n%s", want, logs)
	}
	if strings.Contains(logs, "hot retention disabled") {
		t.Fatalf("hot retention is 14 days here; only the offsite gate is disabled, logged:\n%s", logs)
	}
}

// The other half of the offsite gate: the flag, not the horizon, and the line
// must name the flag so the operator knows which setting to change.
func TestRunOnceReportsDisabledOffsitePruningFlag(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	id, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeRsync, OffsiteRemote: "local",
		RetentionDays: 14, OffsiteRetentionDays: 90, Enabled: true,
	})
	// Set through the admin path, because CreateClient does not carry the flag
	// and this is how an operator ticks "disable offsite pruning" in the first
	// place.
	if err := st.UpdateClientOffsite(context.Background(), id, "local", "", "", "", true); err != nil {
		t.Fatalf("disable offsite pruning: %v", err)
	}
	mkRsyncSnapshots(t, dir, "20260618T100000Z", "20260618T110000Z")

	logs := runOnceCapturingLog(t, Deps{
		Store: st, Offsite: &fakeOffsite{}, BackupBaseDir: base,
		Now: func() time.Time { return now },
	})

	want := `offsite retention disabled for client "c" (offsite_pruning_disabled=true): no objects will be pruned`
	if !strings.Contains(logs, want) {
		t.Fatalf("a pass with offsite pruning disabled must name the flag;\nwant: %s\ngot:\n%s", want, logs)
	}
}

// Same class on the hot side: retention_days=0 means pruneHot always returns
// early, so the pass has to say so.
func TestRunOnceReportsDisabledHotRetention(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	_, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeRsync, OffsiteRemote: "local",
		RetentionDays: 0, OffsiteRetentionDays: 90, Enabled: true,
	})
	mkRsyncSnapshots(t, dir, "20260618T100000Z", "20260618T110000Z")

	logs := runOnceCapturingLog(t, Deps{
		Store: st, Offsite: &fakeOffsite{}, BackupBaseDir: base,
		Now: func() time.Time { return now },
	})

	want := `hot retention disabled for client "c" (retention_days=0): no snapshots will be pruned`
	if !strings.Contains(logs, want) {
		t.Fatalf("a pass with retention_days=0 must say why nothing is pruned;\nwant: %s\ngot:\n%s", want, logs)
	}
	if strings.Contains(logs, "offsite retention disabled") {
		t.Fatalf("offsite retention is 90 days here; only the hot gate is disabled, logged:\n%s", logs)
	}
}

// No noise for the healthy case: both horizons set and pruning enabled means the
// pass has nothing to explain, and a line on every pass for every client would
// be exactly the sort of permanent chatter that hides the signal.
func TestRunOnceHealthyClientLogsNoRetentionDisabledLine(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	_, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeRsync, OffsiteRemote: "local",
		RetentionDays: 14, OffsiteRetentionDays: 90, Enabled: true,
	})
	mkRsyncSnapshots(t, dir, "20260618T080000Z", "20260618T100000Z", "20260618T110000Z")

	logs := runOnceCapturingLog(t, Deps{
		Store: st, Offsite: &fakeOffsite{}, BackupBaseDir: base,
		Now: func() time.Time { return now },
	})

	if strings.Contains(logs, "retention disabled") {
		t.Fatalf("both horizons are set and pruning is enabled; nothing may claim otherwise:\n%s", logs)
	}
}

// pruneOffsite is only reached for a client that has offsite configured at all.
// Without a remote there is nothing to prune and nothing to explain, so the line
// must not appear.
func TestRunOnceNoOffsiteRemoteLogsNoOffsiteRetentionLine(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	_, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeRsync,
		RetentionDays: 14, OffsiteRetentionDays: 0, Enabled: true,
	})
	mkRsyncSnapshots(t, dir, "20260618T100000Z", "20260618T110000Z")

	logs := runOnceCapturingLog(t, Deps{
		Store: st, Offsite: &fakeOffsite{}, BackupBaseDir: base,
		Now: func() time.Time { return now },
	})

	if strings.Contains(logs, "offsite retention disabled") {
		t.Fatalf("no offsite remote is configured, so offsite retention cannot be disabled; logged:\n%s", logs)
	}
}
