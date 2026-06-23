// Package authkeys renders and atomically writes the sshd authorized_keys file
// from the client list (design doc D4 + the outside-voice hardening).
//
// Each enabled client gets one line with a per-mode FORCED command, locked to
// that client's own directory, so the SSH key can do nothing but that client's
// backup into that client's directory:
//
//	rsync : restrict,command="rrsync <baseDir>/<id>"        <pubkey> backitup:<id>
//	targz : restrict,command="backitup-recv <baseDir>/<id>" <pubkey> backitup:<id>
//
// Confinement comes from the command itself (rrsync's path jail; the receiver
// script's hardcoded directory), NOT from client-supplied input — the directory
// is derived from the integer client ID, never from anything the client sends.
//
// Injection defense: a public key is only emitted if it parses as a single,
// well-formed authorized_keys entry with no embedded newlines or quotes. A
// malformed/poisoned row is SKIPPED (that client loses access) rather than
// failing the whole file — one bad row must not break auth for the fleet.
package authkeys

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/th0rn0/backitup/internal/model"
)

// Header is written at the top of every generated file. The file is fully
// regenerated from the DB on each change; do not hand-edit.
const Header = "# Managed by backitup. Do not edit; regenerated from the database.\n"

// Skipped records a client whose key was rejected and why.
type Skipped struct {
	ClientID int64
	Reason   string
}

// Render builds the authorized_keys body for all ENABLED clients with a key,
// confining each to baseDir/<slug>. Invalid entries are skipped and returned.
func Render(clients []model.Client, baseDir string) (content string, skipped []Skipped) {
	var b strings.Builder
	b.WriteString(Header)

	sorted := append([]model.Client(nil), clients...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	for _, c := range sorted {
		if !c.Enabled || strings.TrimSpace(c.SSHPubKey) == "" {
			continue
		}
		if reason := invalidPubKey(c.SSHPubKey); reason != "" {
			skipped = append(skipped, Skipped{ClientID: c.ID, Reason: reason})
			continue
		}
		cmd, ok := forcedCommand(c.Mode, baseDir, model.Slug(c.Name))
		if !ok {
			skipped = append(skipped, Skipped{ClientID: c.ID, Reason: "unknown mode " + string(c.Mode)})
			continue
		}
		// restrict = drop all forwarding/pty/agent; command = the only thing this
		// key may run. The pubkey is validated above, so it is safe to embed.
		fmt.Fprintf(&b, "restrict,command=\"%s\" %s\n", cmd, strings.TrimSpace(c.SSHPubKey))
	}
	return b.String(), skipped
}

func forcedCommand(m model.Mode, baseDir, slug string) (string, bool) {
	dir := filepath.Join(baseDir, slug)
	switch m {
	case model.ModeRsync:
		// rrsync confines reads+writes to dir (needed for --link-dest + latest).
		return "rrsync " + dir, true
	case model.ModeTarGz:
		// Forced receiver writes the streamed archive into dir; no sftp/chroot
		// needed, and the client cannot touch any other path.
		return "backitup-recv " + dir, true
	default:
		return "", false
	}
}

// invalidPubKey returns a non-empty reason if key is not a safe single
// authorized_keys entry. Guards against ForceCommand injection via embedded
// newlines/quotes and against malformed keys.
func invalidPubKey(key string) string {
	if strings.ContainsAny(key, "\n\r") {
		return "embedded newline"
	}
	if strings.Contains(key, "\"") {
		return "embedded quote"
	}
	for _, r := range key {
		if r < 0x20 { // control characters
			return "control character"
		}
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(key)); err != nil {
		return "unparseable public key"
	}
	return ""
}

// WriteAtomic writes content to path via a temp file in the same directory and
// a rename, so sshd never reads a half-written authorized_keys file (D4).
func WriteAtomic(path, content string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".authorized_keys-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op if the rename succeeded

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
