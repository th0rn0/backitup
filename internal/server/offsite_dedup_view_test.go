package server

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

// A dedup record is not an offsite object: it names no remote path of its own,
// it is never verified (so it would read "not checked" forever), and Download or
// Delete on it address a file that was never uploaded — rclone always errors.
// The client page must offer neither, and must not count it as an object.
func TestClientPageListsOnlyRealOffsiteObjects(t *testing.T) {
	st, ts, _ := ingestStack(t)
	ctx := context.Background()
	if _, err := st.CreateClient(ctx, model.Client{
		Name: "dedup-page", Mode: model.ModeTarGz, RetentionDays: 14,
		OffsiteRemote: "s3", Enabled: true,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	c, err := st.GetClientBySlug(ctx, "dedup-page")
	if err != nil || c == nil {
		t.Fatalf("load client: %v", err)
	}

	now := time.Now().UTC()
	if err := st.RecordOffsiteObject(ctx, model.OffsiteObject{
		ClientID: c.ID, SnapshotID: "real.tar.gz", Remote: "s3", Bytes: 42, UploadedAt: now,
	}); err != nil {
		t.Fatalf("record real object: %v", err)
	}
	if err := st.RecordOffsiteObject(ctx, model.OffsiteObject{
		ClientID: c.ID, SnapshotID: "dup.tar.gz", Remote: "s3", UploadedAt: now, DedupOf: "real.tar.gz",
	}); err != nil {
		t.Fatalf("record dedup row: %v", err)
	}

	cl := loggedInClient(t, st, ts)
	resp, err := cl.Get(ts.URL + "/clients/dedup-page")
	if err != nil {
		t.Fatalf("get detail: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("detail status = %d, want 200", resp.StatusCode)
	}
	html := string(body)

	if !strings.Contains(html, "real.tar.gz") {
		t.Fatal("the real offsite object is missing from the page")
	}
	if strings.Contains(html, "dup.tar.gz") {
		t.Error("a dedup record is rendered as an offsite object: its Download/Delete address a path that was never uploaded")
	}
	if !strings.Contains(html, ">1 object<") {
		t.Error("the offsite object count includes dedup records; they are records, not objects")
	}
}
