package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"
)

// installListingRclone puts a stub `rclone` first on PATH that answers `lsf`
// with exactly these filenames and records every invocation in a log file.
// stdout carries only the listing, because that is what lifecycle.Rclone parses.
func installListingRclone(t *testing.T, names []string) {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "rclone-calls.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + calls + "\n" +
		"case \"$*\" in\n*' lsf '*) printf '%s\\n' " + shellQuoteAll(names) + " ;;\nesac\nexit 0\n"
	bin := filepath.Join(dir, "rclone")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("install listing rclone: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// shellQuoteAll renders words as a single-quoted argument list for /bin/sh.
func shellQuoteAll(words []string) string {
	if len(words) == 0 {
		return "''"
	}
	out := make([]string, 0, len(words))
	for _, w := range words {
		out = append(out, "'"+strings.ReplaceAll(w, "'", `'\''`)+"'")
	}
	return strings.Join(out, " ")
}

func TestOffsiteObjectPathFollowsTheUploadMode(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	srv := New(st, false)
	ctx := context.Background()

	const targzSnap = "backup-20261124T110000Z-000.tar.gz"
	const rsyncSnap = "20261124T110000Z"

	tests := []struct {
		name       string
		client     *model.Client
		snapshotID string
		wantPath   string
		wantName   string
	}{
		{
			// No prefix chosen and no fleet default: the client's slug.
			"all/targz prefixes the per-snapshot name with the slug",
			&model.Client{Slug: "laptop", Mode: model.ModeTarGz, OffsiteUploadMode: model.OffsiteUploadAll},
			targzSnap, "laptop/laptop-" + targzSnap, "laptop-" + targzSnap,
		},
		{
			"latest/targz prefixes the per-snapshot name with the slug",
			&model.Client{Slug: "laptop", Mode: model.ModeTarGz, OffsiteUploadMode: model.OffsiteUploadLatest},
			targzSnap, "laptop/laptop-" + targzSnap, "laptop-" + targzSnap,
		},
		{
			"all/rsync archives under the prefixed snapshot name",
			&model.Client{Slug: "laptop", Mode: model.ModeRsync, OffsiteUploadMode: model.OffsiteUploadAll},
			rsyncSnap, "laptop/laptop-" + rsyncSnap + ".tar.gz", "laptop-" + rsyncSnap + ".tar.gz",
		},
		{
			// Single generation keeps ONE fixed name; the prefix goes in front of
			// it, which keeps the name fixed across uploads (the property the mode
			// depends on) and still identifies the file's owner.
			"single/targz fetches the prefixed fixed object",
			&model.Client{Slug: "laptop", Mode: model.ModeTarGz, OffsiteUploadMode: model.OffsiteUploadSingle},
			targzSnap, "laptop/laptop-" + model.SingleObjectName, "laptop-" + model.SingleObjectName,
		},
		{
			"single/rsync fetches the prefixed fixed object",
			&model.Client{Slug: "laptop", Mode: model.ModeRsync, OffsiteUploadMode: model.OffsiteUploadSingle},
			rsyncSnap, "laptop/laptop-" + model.SingleObjectName, "laptop-" + model.SingleObjectName,
		},
		{
			// A client's own value wins over the slug, and only the NAME moves:
			// the directory component is untouched.
			"a per-client override replaces the slug",
			&model.Client{Slug: "laptop", Mode: model.ModeTarGz, OffsiteUploadMode: model.OffsiteUploadAll,
				NamePrefix: "nas", NamePrefixSet: true},
			targzSnap, "laptop/nas-" + targzSnap, "nas-" + targzSnap,
		},
		{
			// "No prefix" is the old names exactly, which is what an operator who
			// wants nothing but the snapshot id gets.
			"an explicit no prefix keeps the historic name",
			&model.Client{Slug: "laptop", Mode: model.ModeTarGz, OffsiteUploadMode: model.OffsiteUploadAll,
				NamePrefixSet: true},
			targzSnap, "laptop/" + targzSnap, targzSnap,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// No remote row: the path keeps its historic shape, which is what a
			// hand-written rclone section gets.
			gotPath, gotName, err := srv.offsiteObjectPath(ctx, tc.client, "laptop", tc.snapshotID)
			if err != nil {
				t.Fatalf("resolve path: %v", err)
			}
			if gotPath != tc.wantPath {
				t.Errorf("path = %q, want %q", gotPath, tc.wantPath)
			}
			if gotName != tc.wantName {
				t.Errorf("object name for the download filename = %q, want %q", gotName, tc.wantName)
			}
		})
	}
}

// The fleet default is what a client that has chosen nothing carries, and it is
// how a whole fleet is named consistently without touching every client.
func TestOffsiteObjectPathUsesTheFleetDefault(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	srv := New(st, false)
	srv.ConfigureNamePrefixDefault("backitup")

	const snap = "backup-20261124T110000Z-000.tar.gz"
	c := &model.Client{Slug: "laptop", Mode: model.ModeTarGz, OffsiteUploadMode: model.OffsiteUploadAll}
	if _, name, err := srv.offsiteObjectPath(context.Background(), c, "laptop", snap); err != nil {
		t.Fatalf("resolve path: %v", err)
	} else if name != "backitup-"+snap {
		t.Errorf("name = %q, want %q", name, "backitup-"+snap)
	}

	// ... until a client says otherwise.
	c.NamePrefix, c.NamePrefixSet = "nas", true
	if _, name, err := srv.offsiteObjectPath(context.Background(), c, "laptop", snap); err != nil {
		t.Fatalf("resolve path: %v", err)
	} else if name != "nas-"+snap {
		t.Errorf("name = %q, want the client's own %q", name, "nas-"+snap)
	}
}

// The compatibility problem, at the download route: a bucket written before name
// prefixes existed holds "laptop/<snapshot id>". Verification counts it as
// present, so a download must fetch THAT object — addressing the new name would
// stream rclone's "not found" into the operator's browser.
func TestOffsiteObjectPathFallsBackToTheLegacyName(t *testing.T) {
	const snap = "backup-20261124T110000Z-000.tar.gz"
	installRcloneStub(t, "")

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	srv := New(st, false)
	srv.ConfigureRclone(filepath.Join(t.TempDir(), "rclone.conf"))

	c := &model.Client{Slug: "laptop", Mode: model.ModeTarGz, OffsiteUploadMode: model.OffsiteUploadAll}
	ctx := context.Background()

	// The plain stub lists nothing, which is the "could not ask" case: the
	// current name is the right guess and the caller reports rclone's own error.
	if _, name, err := srv.offsiteObjectPath(ctx, c, "laptop", snap); err != nil {
		t.Fatalf("resolve path: %v", err)
	} else if name != "laptop-"+snap {
		t.Fatalf("with an empty listing the name = %q, want the current one %q", name, "laptop-"+snap)
	}

	// Now the bucket really contains the legacy object.
	installListingRclone(t, []string{snap})
	if _, name, err := srv.offsiteObjectPath(ctx, c, "laptop", snap); err != nil {
		t.Fatalf("resolve path: %v", err)
	} else if name != snap {
		t.Fatalf("name = %q; the legacy object that is really in the bucket must win over the one that is not", name)
	}

	// And when both exist, the prefixed one — the one this build writes — wins.
	installListingRclone(t, []string{snap, "laptop-" + snap})
	if _, name, err := srv.offsiteObjectPath(ctx, c, "laptop", snap); err != nil {
		t.Fatalf("resolve path: %v", err)
	} else if name != "laptop-"+snap {
		t.Fatalf("name = %q; the current name must win when both spellings exist", name)
	}
}
