package authkeys

import (
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/model"
)

// The jail path must come from the persisted slug. Recomputing it from the name
// is what let two clients land in one directory (S2/S3): the stored value is the
// one the rest of the server uses for snapshots, offsite objects and downloads.
func TestRenderJailsToStoredSlug(t *testing.T) {
	key := freshKey(t)
	clients := []model.Client{
		{ID: 7, Name: "Laptop Docs", Slug: "client-7", Mode: model.ModeRsync, SSHPubKey: key, Enabled: true},
	}
	content, skipped := Render(clients, "/srv/backups")
	if len(skipped) != 0 {
		t.Fatalf("unexpected skips: %+v", skipped)
	}
	if !strings.Contains(content, `command="rrsync /srv/backups/client-7"`) {
		t.Fatalf("forced command does not use the stored slug:\n%s", content)
	}
	if strings.Contains(content, "laptop-docs") {
		t.Fatalf("forced command recomputed the path from the client name:\n%s", content)
	}
}

// A client without a slug has no directory of its own; emitting a line for it
// would either point at the shared base dir or collide with another client.
func TestRenderSkipsEmptySlug(t *testing.T) {
	clients := []model.Client{
		{ID: 1, Name: "..", Slug: "", Mode: model.ModeRsync, SSHPubKey: freshKey(t), Enabled: true},
		{ID: 2, Name: "ok", Slug: "ok", Mode: model.ModeRsync, SSHPubKey: freshKey(t), Enabled: true},
	}
	content, skipped := Render(clients, "/srv/backups")
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, "rrsync") && strings.Contains(line, "backups\"") {
			t.Fatalf("emitted a jail at the base directory itself:\n%s", content)
		}
	}
	if strings.Count(strings.TrimSpace(content), "\n") != 1 {
		t.Fatalf("expected exactly one key line (the client with a slug):\n%s", content)
	}
	var found bool
	for _, sk := range skipped {
		if sk.ClientID == 1 && strings.Contains(sk.Reason, "slug") {
			found = true
		}
	}
	if !found {
		t.Fatalf("client with an empty slug was not reported as skipped: %+v", skipped)
	}
}

// Defense in depth: even a tampered slug must never break out of the jail a
// different client owns.
func TestRenderSkipsSlugThatEscapesBaseDir(t *testing.T) {
	for _, slug := range []string{"..", ".", "../evil", "a/b", `a\b`, "/abs"} {
		clients := []model.Client{
			{ID: 1, Name: "evil", Slug: slug, Mode: model.ModeTarGz, SSHPubKey: freshKey(t), Enabled: true},
		}
		content, skipped := Render(clients, "/srv/backups")
		if strings.Contains(content, "backitup-recv") {
			t.Fatalf("slug %q produced a forced command:\n%s", slug, content)
		}
		if len(skipped) != 1 {
			t.Fatalf("slug %q was not reported as skipped: %+v", slug, skipped)
		}
	}
}

// The invariant this whole card exists for: two clients never receive the same
// forced command, hence never share one read-write directory.
func TestRenderGivesEveryClientItsOwnJail(t *testing.T) {
	clients := []model.Client{
		{ID: 1, Name: "dup a", Slug: "dup-a", Mode: model.ModeRsync, SSHPubKey: freshKey(t), Enabled: true},
		{ID: 2, Name: "dup-a", Slug: "dup-a-2", Mode: model.ModeRsync, SSHPubKey: freshKey(t), Enabled: true},
	}
	content, skipped := Render(clients, "/srv/backups")
	if len(skipped) != 0 {
		t.Fatalf("unexpected skips: %+v", skipped)
	}
	var cmds []string
	for _, line := range strings.Split(content, "\n") {
		if i := strings.Index(line, `command="rrsync `); i >= 0 {
			cmds = append(cmds, line[i:])
		}
	}
	if len(cmds) != 2 {
		t.Fatalf("expected two forced commands, got %d:\n%s", len(cmds), content)
	}
	if cmds[0] == cmds[1] {
		t.Fatalf("two clients share one jail: %q", cmds[0])
	}
}
