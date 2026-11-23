package lifecycle

import (
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

// A retention horizon of 0 turns hot pruning off, and before this the pass
// returned in silence: from the outside "retention is 0" looked identical to
// "nothing is old enough yet", which is the wrong answer to give an operator
// watching the hot store grow. One line per pass per client, naming the setting
// responsible.
//
// The offsite half of that pair went with the offsite horizon itself: nothing in
// a lifecycle pass expires an offsite object, so there is no offsite setting left
// to name. The hot line must still be there, and must not mention offsite.
func TestRunOnceReportsDisabledHotRetention(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	_, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeRsync, OffsiteRemote: "local",
		RetentionDays: 0, Enabled: true,
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
	if strings.Contains(logs, "offsite retention") {
		t.Fatalf("offsite retention is the bucket's rule, not a setting here, so no line may name it:\n%s", logs)
	}
}

// No noise for the healthy case: nothing is switched off, so the pass has nothing
// to explain, and a line on every pass for every client would be exactly the sort
// of permanent chatter that hides the signal.
func TestRunOnceHealthyClientLogsNoRetentionDisabledLine(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	_, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeRsync, OffsiteRemote: "local",
		RetentionDays: 14, Enabled: true,
	})
	mkRsyncSnapshots(t, dir, "20260618T080000Z", "20260618T100000Z", "20260618T110000Z")

	logs := runOnceCapturingLog(t, Deps{
		Store: st, Offsite: &fakeOffsite{}, BackupBaseDir: base,
		Now: func() time.Time { return now },
	})

	if strings.Contains(logs, "retention disabled") {
		t.Fatalf("nothing is switched off here; nothing may claim otherwise:\n%s", logs)
	}
}

// The pass used to explain, per client per pass, that offsite retention was
// switched off — once when the horizon was 0 and once when the "disable offsite
// pruning" flag was set. Both settings are gone. The case that produced those
// lines (a client with a remote and offsite records past any horizon) must now
// produce no retention line at all: silence means "there is nothing of that kind
// to explain", not "the check did not run".
func TestRunOnceLogsNoOffsiteRetentionLineEvenWithOldObjects(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	id, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeRsync, OffsiteRemote: "local",
		RetentionDays: 14, Enabled: true,
	})
	mkRsyncSnapshots(t, dir, "20260618T100000Z", "20260618T110000Z")
	for _, o := range []model.OffsiteObject{
		{ClientID: id, SnapshotID: "ancient", Remote: "local", UploadedAt: now.Add(-400 * 24 * time.Hour)},
		{ClientID: id, SnapshotID: "old", Remote: "local", UploadedAt: now.Add(-40 * 24 * time.Hour)},
		{ClientID: id, SnapshotID: "fresh", Remote: "local", UploadedAt: now.Add(-time.Hour)},
	} {
		if err := st.RecordOffsiteObject(t.Context(), o); err != nil {
			t.Fatal(err)
		}
	}

	logs := runOnceCapturingLog(t, Deps{
		Store: st, Offsite: &fakeOffsite{}, BackupBaseDir: base,
		Now: func() time.Time { return now },
	})

	if strings.Contains(logs, "offsite retention") || strings.Contains(logs, "retention disabled") {
		t.Fatalf("offsite retention is the bucket's rule; a lifecycle pass must not talk about it:\n%s", logs)
	}
}
