package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

// The operator's view of a failed run. A run that failed because rsync could not
// read four .qcow2 files has to be visible as `failed` with rsync's own error
// text on it — not as a green row with no explanation, and not as a run whose
// counts suggest something was stored.
func TestDashboardShowsFailedRunWithItsErrorText(t *testing.T) {
	st, ts := testStack(t)
	ctx := context.Background()
	now := time.Now().UTC()

	id, err := st.CreateClient(ctx, model.Client{
		Name: "vmhost", Mode: model.ModeRsync, RetentionDays: 14, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	const errText = `rsync: exit status 23: rsync: [sender] send_files failed to open "/source/drone-runner.qcow2": Permission denied (13)`
	logTail := "failed run: rsync counted 5 files / 0 B in the attempted snapshot — NOT a backup, nothing from it is stored\n" +
		"run failed: " + errText + "\n" +
		"snapshots/20261123T221004Z marked incomplete: it is ignored by the listing, checks and offsite upload; `latest` still points at the previous good snapshot"
	runID, err := st.RecordRun(ctx, model.Run{
		ClientID: id, StartedAt: now.Add(-4 * time.Minute), FinishedAt: now.Add(-time.Minute),
		Status: model.StatusFailed, Bytes: 0, Files: 0, LogTail: logTail,
	})
	if err != nil {
		t.Fatalf("record run: %v", err)
	}

	c := loggedInClient(t, st, ts)
	slug := model.Slug("vmhost")

	// The dashboard row: the client is Failed (sorted to the top band), not OK.
	dash := getBody(t, c, ts.URL+"/")
	if !strings.Contains(dash, "vmhost") || !strings.Contains(dash, "Failed") {
		t.Fatalf("dashboard must show the client as Failed:\n%s", dash)
	}

	// The client page: the run itself reads `failed`.
	detail := getBody(t, c, ts.URL+"/clients/"+slug)
	if !strings.Contains(detail, "failed") {
		t.Fatalf("client page must show the run as failed:\n%s", detail)
	}

	// The run log page: rsync's own explanation, which is what an operator acts
	// on (fix the ownership of the .qcow2 files, then re-run).
	runLog := getBody(t, c, fmt.Sprintf("%s/clients/%s/runs/%d", ts.URL, slug, runID))
	for _, want := range []string{"Permission denied (13)", "/source/drone-runner.qcow2", "NOT a backup", "marked incomplete"} {
		if !strings.Contains(runLog, want) {
			t.Errorf("run log page must show %q:\n%s", want, runLog)
		}
	}
	// And it must not present the attempt as a completed sync.
	if strings.Contains(runLog, "synced ") {
		t.Errorf("the run log must not read like a completed sync:\n%s", runLog)
	}
}

func getBody(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return string(body)
}
