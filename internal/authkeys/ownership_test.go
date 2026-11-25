package authkeys

import (
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/model"
)

// The setting's effect is the forced command, so this file is where "off is
// today's behaviour" has to be provable: an existing client's line must not
// change at all.
func TestRenderKeepsRrsyncForAClientThatDoesNotRecordOwnership(t *testing.T) {
	key := freshKey(t)
	clients := []model.Client{
		{ID: 1, Name: "plain", Slug: "plain", Mode: model.ModeRsync, SSHPubKey: key, Enabled: true},
	}
	content, skipped := Render(clients, "/srv/backups")
	if len(skipped) != 0 {
		t.Fatalf("unexpected skips: %+v", skipped)
	}
	want := `restrict,command="rrsync /srv/backups/plain" ` + key
	if !strings.Contains(content, want) {
		t.Fatalf("an ownership-off client must keep the plain rrsync command:\nwant line: %s\ngot:\n%s", want, content)
	}
	if strings.Contains(content, "backitup-rrsync") {
		t.Fatalf("backitup-rrsync leaked into a client that did not ask for it:\n%s", content)
	}
}

func TestRenderUsesTheOwnershipWrapperWhenTheClientRecordsIt(t *testing.T) {
	key := freshKey(t)
	clients := []model.Client{
		{ID: 1, Name: "owned", Slug: "owned", Mode: model.ModeRsync, SSHPubKey: key, Enabled: true, RecordOwnership: true},
	}
	content, skipped := Render(clients, "/srv/backups")
	if len(skipped) != 0 {
		t.Fatalf("unexpected skips: %+v", skipped)
	}
	want := `restrict,command="backitup-rrsync /srv/backups/owned" ` + key
	if !strings.Contains(content, want) {
		t.Fatalf("want %q in:\n%s", want, content)
	}
	// The jail is the same one: the wrapper execs rrsync with it, so the
	// confinement must not depend on which command was chosen.
	if strings.Contains(content, `command="rrsync /srv/backups/owned"`) {
		t.Fatalf("both commands for one client:\n%s", content)
	}
}

// A tar.gz client's receiver is not rsync at all: the setting is refused at the
// handler, and the renderer must not hand it a command that does not exist for
// its mode even if a row says so.
func TestRenderIgnoresOwnershipForTarGz(t *testing.T) {
	key := freshKey(t)
	clients := []model.Client{
		{ID: 1, Name: "tgz", Slug: "tgz", Mode: model.ModeTarGz, SSHPubKey: key, Enabled: true, RecordOwnership: true},
	}
	content, _ := Render(clients, "/srv/backups")
	if !strings.Contains(content, `restrict,command="backitup-recv /srv/backups/tgz" `+key) {
		t.Fatalf("targz client lost its receiver command:\n%s", content)
	}
	if strings.Contains(content, "backitup-rrsync") {
		t.Fatalf("an rsync wrapper was rendered for a targz client:\n%s", content)
	}
}
