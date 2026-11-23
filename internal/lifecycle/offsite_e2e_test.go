package lifecycle

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

// TestOffsiteEndToEndThroughTheLocalBackend is the cheap end-to-end proof of
// the offsite path: upload → dedupe → verify → decrypt, against a real rclone
// and a real (encrypted) object store instead of a stub.
//
// It needs no B2 account because the provider stand-in is rclone's `local`
// backend, addressed through a directory this test owns. That backend is the
// reason model.BackendLocal exists: it is deliberately absent from
// model.Backends (no form offers it) and refused by the create handler, so the
// only way to configure one is from Go code — a test.
//
// The test skips when rclone is not installed. Where it is (and the project's
// own checks run with it on PATH), this is the one test that proves a backup
// really lands on the provider as ciphertext and comes back as the original
// bytes.
func TestOffsiteEndToEndThroughTheLocalBackend(t *testing.T) {
	rcloneBin, err := exec.LookPath("rclone")
	if err != nil {
		t.Skip("rclone is not on PATH: install it to run the offsite end-to-end proof (upload → dedupe → verify → decrypt)")
	}
	st := openStore(t)
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

	// The provider stand-in: a directory this test owns. It is configured as the
	// remote's bucket, which is how a B2 bucket is addressed too.
	providerDir := filepath.Join(t.TempDir(), "provider")
	if err := os.MkdirAll(providerDir, 0o755); err != nil {
		t.Fatalf("mkdir provider: %v", err)
	}
	obscure := func(secret string) string {
		t.Helper()
		cmd := exec.Command(rcloneBin, "obscure", secret)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("rclone obscure: %v", err)
		}
		return strings.TrimSpace(string(out))
	}

	plaintext := model.Remote{
		Name: "fakecloud", Backend: model.BackendLocal,
		Config:    map[string]string{"bucket": providerDir},
		CreatedAt: now,
	}
	vault := model.Remote{
		Name: "vault", Backend: model.BackendCrypt,
		Config: map[string]string{
			"remote":                    "fakecloud",
			"filename_encryption":       "standard",
			"directory_name_encryption": "true",
			"password":                  obscure("e2e-password-must-never-leak"),
			"password2":                 obscure("e2e-salt-must-never-leak"),
		},
		CreatedAt: now,
	}
	for _, r := range []model.Remote{plaintext, vault} {
		if err := st.CreateRemote(t.Context(), r); err != nil {
			t.Fatalf("create remote %q: %v", r.Name, err)
		}
	}

	// rclone.conf exactly as the server writes it: one section per row, with the
	// crypt pointing at its parent's own root path.
	confPath := filepath.Join(t.TempDir(), "rclone.conf")
	byName := map[string]model.Remote{plaintext.Name: plaintext, vault.Name: vault}
	lookup := func(name string) (model.Remote, bool) { r, ok := byName[name]; return r, ok }
	conf := plaintext.RcloneSectionFor(lookup) + "\n" + vault.RcloneSectionFor(lookup)
	if err := os.WriteFile(confPath, []byte(conf), 0o600); err != nil {
		t.Fatalf("write rclone.conf: %v", err)
	}
	if !strings.Contains(conf, "type = crypt") || !strings.Contains(conf, "remote = fakecloud:"+providerDir) {
		t.Fatalf("the generated config does not route the crypt remote through the provider root:\n%s", conf)
	}

	// Three snapshots: two distinct payloads, one a repeat of the first so the
	// dedupe path is exercised.
	_, base, dir := newClient(t, st, model.Client{
		Name: "laptop", Mode: model.ModeTarGz, OffsiteRemote: "vault",
		RetentionDays: 14, Enabled: true,
	})
	tree := func(name, content string, age time.Duration) string {
		t.Helper()
		p := filepath.Join(dir, name+".tar.gz")
		mkArchiveContent(t, p, now.Add(-age), content)
		return p
	}
	// The first two share content *and* mtime, which is what makes the archive
	// bytes — and therefore the checksum — identical.
	a := tree("backup-20261118T090000Z-000", "PAYLOAD-MARKER-alpha", 3*time.Hour)
	b := tree("backup-20261118T100000Z-000", "PAYLOAD-MARKER-beta", 2*time.Hour)
	c := tree("backup-20261118T110000Z-000", "PAYLOAD-MARKER-alpha", 3*time.Hour)

	deps := Deps{
		Store:         st,
		Offsite:       NewRclone(confPath),
		BackupBaseDir: base,
		Now:           func() time.Time { return now },
	}
	if err := RunOffsiteOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOffsiteOnce: %v", err)
	}

	clientID, err := st.GetClientBySlug(t.Context(), "laptop")
	if err != nil || clientID == nil {
		t.Fatalf("load client: %v (client=%v)", err, clientID)
	}
	objs, err := st.ListOffsiteObjects(t.Context(), clientID.ID)
	if err != nil {
		t.Fatalf("list offsite objects: %v", err)
	}
	if len(objs) != 3 {
		t.Fatalf("offsite records = %d, want 3 (two uploads, one dedup): %+v", len(objs), objs)
	}
	var uploaded, deduped int
	for _, o := range objs {
		if o.DedupOf != "" {
			deduped++
			if o.Bytes != 0 {
				t.Errorf("dedup row %s recorded %d bytes; nothing was uploaded", o.SnapshotID, o.Bytes)
			}
			continue
		}
		uploaded++
	}
	if uploaded != 2 || deduped != 1 {
		t.Fatalf("uploads=%d dedup=%d, want 2 and 1: %+v", uploaded, deduped, objs)
	}

	// ── what the provider actually holds ────────────────────────────────────
	var providerFiles []string
	err = filepath.WalkDir(providerDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			providerFiles = append(providerFiles, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk provider dir: %v", err)
	}
	if len(providerFiles) != 2 {
		t.Fatalf("the provider holds %d objects, want 2 (one per distinct payload): %v", len(providerFiles), providerFiles)
	}
	for _, p := range providerFiles {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read provider object: %v", err)
		}
		if bytes.Contains(raw, []byte("PAYLOAD-MARKER")) {
			t.Fatalf("the provider holds the backup in the clear (%s): offsite encryption is not in effect", p)
		}
		if bytes.Equal(raw, mustRead(t, a)) || bytes.Equal(raw, mustRead(t, b)) {
			t.Fatalf("provider object %s is byte-identical to a hot archive: nothing was encrypted", p)
		}
	}

	// ── verify, then decrypt through rclone ────────────────────────────────
	if err := RunOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	objs, err = st.ListOffsiteObjects(t.Context(), clientID.ID)
	if err != nil {
		t.Fatalf("list offsite objects after verify: %v", err)
	}
	for _, o := range objs {
		if o.DedupOf != "" {
			// A dedup row names the owner's object, and verifying the owner
			// covers it; it is never verified itself (see offsitedSet).
			continue
		}
		if o.RemoteVerifiedAt.IsZero() {
			t.Errorf("offsite object %s was never verified after a lifecycle pass: %+v", o.SnapshotID, o)
		}
		if o.RemoteMissing {
			t.Errorf("offsite object %s reported missing although it was just uploaded", o.SnapshotID)
		}
	}

	// The decrypted read must give back the original bytes — that is the whole
	// point of the crypt wrapper, and the only way to prove the password stored
	// in the database is the one the objects were written with.
	for _, tc := range []struct{ name, archive string }{
		{"backup-20261118T090000Z-000.tar.gz", a},
		{"backup-20261118T100000Z-000.tar.gz", b},
		{"backup-20261118T110000Z-000.tar.gz", c},
	} {
		// The two identical snapshots share one object: read the owner's.
		owner := tc.name
		for _, o := range objs {
			if o.SnapshotID == tc.name && o.DedupOf != "" {
				owner = o.DedupOf
			}
		}
		objectPath := "laptop/" + owner
		out, err := exec.Command(rcloneBin, "--config", confPath, "cat", "vault:"+objectPath).Output()
		if err != nil {
			t.Fatalf("rclone cat vault:%s: %v", objectPath, err)
		}
		if want := mustRead(t, tc.archive); !bytes.Equal(out, want) {
			t.Fatalf("decrypted object for %s does not match the hot archive (%d vs %d bytes)",
				tc.name, len(out), len(want))
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
