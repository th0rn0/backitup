package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"

	// register the server-side modes, as cmd/server does
	_ "github.com/th0rn0/backitup/internal/mode/rsync"
	_ "github.com/th0rn0/backitup/internal/mode/targz"
)

const (
	// The report's own two snapshots, kept so the assertions read like the defect:
	// one hot copy retention kept, one whose hot copy it pruned because a strictly
	// newer snapshot was already offsite.
	retainedSnap    = "20261125T010000Z"
	offsiteOnlySnap = "20261124T203753Z"
	// A later run over an unchanged tree: deduplicated against offsiteOnlySnap's
	// object, so it has no object of its own.
	dedupedSnap = "20261125T020000Z"
)

// offsiteOnlyFixture builds the state the defect describes: an rsync client with
// a retained snapshot on disk and an offsite record for a snapshot the pruner has
// already deleted. It returns the single offsite row so a test can make the last
// verification report it missing.
func offsiteOnlyFixture(t *testing.T) (*store.Store, *http.Client, *httptest.Server, *model.Client, model.OffsiteObject) {
	t.Helper()
	st, _, ts, base := newDiskStack(t)
	cl := noRedirectClientWithSession(t, st, ts)
	postForm(t, cl, ts.URL+"/clients", url.Values{
		"name": {"nas"}, "mode": {"rsync"}, "retention_days": {"14"}, "offsite_remote": {"s3"},
	})
	c, err := st.GetClientBySlug(context.Background(), "nas")
	if err != nil || c == nil {
		t.Fatalf("load created client: %v (client=%v)", err, c)
	}
	writeSnapshot(t, base, c.Slug, retainedSnap, "data.bin", 1024)
	if err := st.RecordOffsiteObject(context.Background(), model.OffsiteObject{
		ClientID: c.ID, SnapshotID: offsiteOnlySnap, Remote: "s3",
		Bytes: 3200000000, UploadedAt: time.Now().UTC().Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("record the offsite object: %v", err)
	}
	objs, err := st.ListOffsiteObjects(context.Background(), c.ID)
	if err != nil || len(objs) != 1 {
		t.Fatalf("offsite rows = %d, want 1 (err %v)", len(objs), err)
	}
	return st, cl, ts, c, objs[0]
}

// offsiteRow is tableRow scoped to the offsite objects table. The same snapshot
// id now appears in the local snapshot table as well, so a bare id lookup can
// land on the local row and read the wrong badges. The offsite row is the one
// that carries a state badge; since the UX pass removed the constant "on disk"
// badge from the local table, that is what distinguishes the two.
func offsiteRow(t *testing.T, body, id string) string {
	t.Helper()
	needle := `<td class="mono">` + id + `</td>`
	rest := body
	for {
		i := strings.Index(rest, needle)
		if i < 0 {
			t.Fatalf("no offsite row for %s on the page", id)
		}
		start := strings.LastIndex(rest[:i], "<tr")
		end := strings.Index(rest[i:], "</tr>")
		if start < 0 || end < 0 {
			t.Fatalf("could not delimit the row for %s", id)
		}
		row := rest[start : i+end]
		if strings.Contains(row, `class="badge`) {
			return row
		}
		rest = rest[i+len(needle):]
	}
}

// tableRow returns the rendered <tr> whose first mono cell is the snapshot id, so
// an assertion is about that row and not about a word appearing somewhere else on
// a page that renders the same snapshot in two tables.
func tableRow(t *testing.T, body, id string) string {
	t.Helper()
	needle := `<td class="mono">` + id + `</td>`
	i := strings.Index(body, needle)
	if i < 0 {
		t.Fatalf("no table row for %s on the page:\n%s", id, body)
	}
	rest := body[i:]
	if j := strings.Index(rest, "</tr>"); j > 0 {
		return rest[:j]
	}
	return rest
}

// The defect: an offsite-only snapshot (the pruner deleted the hot copy once a
// newer one was offsite) read "missing on disk" in red with a single,
// index-deleting action. It is the designed steady state of a working retention
// policy, so the page must say so, offer the download of the copy that does
// exist, and say why the row is there.
func TestClientPageCallsAPrunedHotCopyOffsiteOnlyNotLost(t *testing.T) {
	st, cl, ts, c, _ := offsiteOnlyFixture(t)
	// The fixture gives only the pruned snapshot an offsite row. Give the
	// retained one a row too, so both states sit in the same table and the words
	// can be compared: it still exists on disk, so it is the "on disk" case.
	if err := st.RecordOffsiteObject(context.Background(), model.OffsiteObject{
		ClientID: c.ID, SnapshotID: retainedSnap, Remote: "s3",
		Bytes: 1024, UploadedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("record the retained snapshot's offsite object: %v", err)
	}
	body := getPage(t, cl, ts.URL+"/clients/"+c.Slug)

	row := tableRow(t, body, offsiteOnlySnap)
	if !strings.Contains(row, ">offsite only<") {
		t.Errorf("the offsite-only row is not badged \"offsite only\":\n%s", row)
	}
	if strings.Contains(row, "missing on disk") {
		t.Errorf("the offsite-only row still reads as data loss:\n%s", row)
	}
	if strings.Contains(row, "badge-missing") {
		t.Errorf("a copy the last verification saw is drawn in the alarm colour:\n%s", row)
	}
	// The one state where the bucket holds the only copy is the one that must be
	// able to hand it over.
	wantDownload := "/clients/" + c.Slug + "/offsite/" + offsiteOnlySnap + "/download"
	if !strings.Contains(row, wantDownload) {
		t.Errorf("the only copy is not downloadable from its own row (want %s):\n%s", wantDownload, row)
	}
	if strings.Contains(row, "Delete entry") {
		t.Errorf("the row still offers the index-deleting action by that name:\n%s", row)
	}
	// Both states are on this page, and they are different words.
	if got := offsiteRow(t, body, retainedSnap); !strings.Contains(got, "on disk") {
		t.Errorf("the retained snapshot is not badged \"on disk\":\n%s", got)
	}
	if !strings.Contains(body, "retention deleted the hot snapshot once a strictly newer one was already offsite") {
		t.Error("the page does not say why an offsite-only row exists")
	}
}

// Only the state that is genuinely in doubt keeps the alarm: a row whose last
// verification could not see the object is not on disk AND not in the bucket, and
// the wording has to say both, or it repeats the original defect in red.
func TestClientPageReservesTheAlarmForAnObjectMissingFromTheBucket(t *testing.T) {
	st, cl, ts, c, obj := offsiteOnlyFixture(t)
	if err := st.UpdateOffsiteRemoteStatus(context.Background(), obj.ID, true, time.Now().UTC()); err != nil {
		t.Fatalf("mark the object missing: %v", err)
	}
	body := getPage(t, cl, ts.URL+"/clients/"+c.Slug)

	row := tableRow(t, body, offsiteOnlySnap)
	if !strings.Contains(row, ">not in the bucket<") {
		t.Errorf("a row whose object the verification could not see is not called out:\n%s", row)
	}
	if !strings.Contains(row, "badge-missing") {
		t.Errorf("the genuinely doubtful row is not in the alarm colour:\n%s", row)
	}
	if strings.Contains(row, ">offsite only<") {
		t.Errorf("an object the verification could not see is still called offsite only:\n%s", row)
	}
	if !strings.Contains(row, "Not on disk") || !strings.Contains(row, "not in the bucket") {
		t.Errorf("the title does not distinguish \"not on disk\" from \"not in the bucket\":\n%s", row)
	}
}

// Forget is not a delete, and the page has to say so twice: in the confirmation
// text, and by requiring a second confirmation that names the snapshot while the
// object is believed to still be in the bucket.
func TestForgetSaysWhatItCostsAndNeedsTheNameOfALiveObject(t *testing.T) {
	st, cl, ts, c, _ := offsiteOnlyFixture(t)
	ctx := context.Background()

	row := tableRow(t, getPage(t, cl, ts.URL+"/clients/"+c.Slug), offsiteOnlySnap)
	for _, want := range []string{"This is NOT a delete", "stays in", "no longer list, download or verify"} {
		if !strings.Contains(row, want) {
			t.Errorf("the confirmation does not state %q:\n%s", want, row)
		}
	}
	wantField := `name="confirm" value="` + offsiteOnlySnap + `"`
	if !strings.Contains(row, wantField) {
		t.Errorf("the form does not carry the second confirmation naming the snapshot (want %s):\n%s", wantField, row)
	}

	// A POST without the name is refused and removes nothing.
	loc := postNoRedirect(t, cl, ts.URL+"/clients/"+c.Slug+"/offsite/"+offsiteOnlySnap+"/forget", url.Values{})
	if !strings.HasPrefix(loc, "/clients/"+c.Slug+"?err=") {
		t.Fatalf("an unconfirmed forget answered %q, want an err flash", loc)
	}
	if !strings.Contains(loc, "not confirmed") || !strings.Contains(loc, offsiteOnlySnap) {
		t.Errorf("the refusal does not name the snapshot it refused to forget: %q", loc)
	}
	if objs, err := st.ListOffsiteObjects(ctx, c.ID); err != nil || len(objs) != 1 {
		t.Fatalf("an unconfirmed forget removed the record: %d rows left (err %v)", len(objs), err)
	}

	// With it, the record goes and the flash says what survived.
	loc = postNoRedirect(t, cl, ts.URL+"/clients/"+c.Slug+"/offsite/"+offsiteOnlySnap+"/forget",
		url.Values{"confirm": {offsiteOnlySnap}})
	if !strings.HasPrefix(loc, "/clients/"+c.Slug+"?msg=") {
		t.Fatalf("a confirmed forget answered %q, want a msg flash", loc)
	}
	if !strings.Contains(loc, "stays in") || !strings.Contains(loc, "no longer lists, downloads or verifies") {
		t.Errorf("the flash does not say that the object remains and tracking ends: %q", loc)
	}
	if objs, err := st.ListOffsiteObjects(ctx, c.ID); err != nil || len(objs) != 0 {
		t.Fatalf("the record survived a confirmed forget: %d rows (err %v)", len(objs), err)
	}
}

// A forgotten record takes the dedupe chain with it: a row that claims "my
// content is in that object" is an inference about an object the app can no
// longer list, download or verify, so nothing may keep claiming it. Stated as
// behaviour: the snapshot the chain named stops counting as offsited, which
// protects its hot copy from the pruner, and the next upload pass gives it an
// object of its own.
func TestForgetDropsTheDedupeChainWithTheRecord(t *testing.T) {
	st, cl, ts, c, _ := offsiteOnlyFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := st.RecordOffsiteObject(ctx, model.OffsiteObject{
		ClientID: c.ID, SnapshotID: dedupedSnap, Remote: "s3",
		UploadedAt: now, DedupOf: offsiteOnlySnap,
	}); err != nil {
		t.Fatalf("record the dedupe row: %v", err)
	}

	loc := postNoRedirect(t, cl, ts.URL+"/clients/"+c.Slug+"/offsite/"+offsiteOnlySnap+"/forget",
		url.Values{"confirm": {offsiteOnlySnap}})
	if !strings.Contains(loc, "1 deduplicated run(s) were dropped with it") {
		t.Errorf("the flash does not say the chain went with the record: %q", loc)
	}

	objs, err := st.ListOffsiteObjects(ctx, c.ID)
	if err != nil {
		t.Fatalf("list offsite objects: %v", err)
	}
	for _, o := range objs {
		if o.DedupOf == offsiteOnlySnap {
			t.Errorf("a dedupe row still points at the record that was just forgotten: %+v", o)
		}
		if o.SnapshotID == dedupedSnap {
			t.Errorf("the deduplicated run still claims an offsite copy nobody can reach: %+v", o)
		}
	}
	if len(objs) != 0 {
		t.Errorf("offsite rows left after forgetting the only record: %+v", objs)
	}
}

// The explanatory line is about rows that exist: a page with only hot copies must
// not carry it.
func TestOffsiteOnlyExplanationAppearsOnlyWithSuchRows(t *testing.T) {
	st, _, ts, base := newDiskStack(t)
	cl := noRedirectClientWithSession(t, st, ts)
	postForm(t, cl, ts.URL+"/clients", url.Values{
		"name": {"docs"}, "mode": {"rsync"}, "retention_days": {"14"}, "offsite_remote": {"s3"},
	})
	c, err := st.GetClientBySlug(context.Background(), "docs")
	if err != nil || c == nil {
		t.Fatalf("load created client: %v (client=%v)", err, c)
	}
	writeSnapshot(t, base, c.Slug, retainedSnap, "data.bin", 1024)
	// The snapshot is offsite AND on disk: nothing is offsite-only.
	if err := st.RecordOffsiteObject(context.Background(), model.OffsiteObject{
		ClientID: c.ID, SnapshotID: retainedSnap, Remote: "s3",
		Bytes: 1024, UploadedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("record the offsite object: %v", err)
	}

	body := getPage(t, cl, ts.URL+"/clients/"+c.Slug)
	if strings.Contains(body, "Rows badged") {
		t.Error("the page explains offsite-only rows it does not have")
	}
	if strings.Contains(tableRow(t, body, retainedSnap), "offsite only") {
		t.Errorf("a snapshot with a hot copy is badged offsite only:\n%s", tableRow(t, body, retainedSnap))
	}
}
