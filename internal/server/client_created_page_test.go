package server

// The create-client and rotate pages are instructions, not records: the operator
// reads them once, on a host that is usually not the server, and the page is the
// only thing they have. These tests assert on the text those pages actually
// render for a given client, in the style of delete_flash_test.go: the defects
// were all in what the page said, so a test that only checked the handlers would
// have stayed green through every one of them.
//
// The environment is the one the card was reproduced in: a tar.gz client called
// nas-photos, BACKITUP_PUBLIC_HOST=backup.example.com:2222 and
// BACKITUP_PUBLIC_API=https://backup.example.com, once with a host key on the
// server and once without.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/store"
)

// pageStack builds an admin-only server with temp ingest paths and, when
// withHostKey is true, a real sshd host public key file, which is what makes the
// page able to offer a verified command at all.
func pageStack(t *testing.T, withHostKey bool) (*Server, *http.Client, *httptest.Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "page.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv := New(st, false)
	srv.dbTimeout = testDBTimeout
	tmp := t.TempDir()
	srv.authKeysPath = filepath.Join(tmp, "authkeys", "authorized_keys")
	srv.backupBaseDir = filepath.Join(tmp, "backups")
	srv.publicHost = "backup.example.com:2222"
	srv.publicAPI = "https://backup.example.com"
	srv.clientImage = "th0rn0/backitup-client:test"
	if withHostKey {
		keyPath := filepath.Join(tmp, "ssh_host_ed25519_key.pub")
		if err := os.WriteFile(keyPath, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA backitup-host\n"), 0o600); err != nil {
			t.Fatalf("write host key: %v", err)
		}
		srv.sshHostKeyPath = keyPath
	}

	setAdmin(t, st, "admin", "pw")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, loginClient(t, ts, "admin", "pw"), ts
}

var (
	textareaRE = regexp.MustCompile(`(?s)<textarea[^>]*id="([^"]+)"[^>]*>(.*?)</textarea>`)
	tokenRE    = regexp.MustCompile(`<input class="secret-input" id="token" value="([^"]+)"`)
)

// fieldText returns the text of the rendered box with the given id, so a test
// reads exactly the characters an operator would copy.
func fieldText(t *testing.T, page, id string) string {
	t.Helper()
	for _, m := range textareaRE.FindAllStringSubmatch(page, -1) {
		if m[1] == id {
			return m[2]
		}
	}
	t.Fatalf("page has no box with id %q", id)
	return ""
}

func pageToken(t *testing.T, page string) string {
	t.Helper()
	m := tokenRE.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("page has no token field")
	}
	return m[1]
}

func createPage(t *testing.T, c *http.Client, ts *httptest.Server, form url.Values) string {
	t.Helper()
	resp, page := mustPostForm(t, c, ts.URL+"/clients", form)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create client = %d, want 200:\n%s", resp.StatusCode, truncate([]byte(page), 2000))
	}
	return page
}

// TestCreatedPageNamesOneSecretsDirectoryAndTheSource is defects 1 and 2: the
// directory named in step 1 and the directory the command mounts are the same
// string, and the read-only source mount carries the client's own source label
// instead of a placeholder that reads like a destination.
func TestCreatedPageNamesOneSecretsDirectoryAndTheSource(t *testing.T) {
	_, c, ts := pageStack(t, true)
	page := createPage(t, c, ts, url.Values{
		"name": {"nas-photos"}, "mode": {"targz"}, "source_label": {"/mnt/photos"},
	})

	const secrets = "/etc/backitup/nas-photos"
	if !strings.Contains(page, "Create <code>"+secrets+"</code>") {
		t.Errorf("step 1 does not name %q as the secrets directory:\n%s", secrets, page)
	}
	cmd := fieldText(t, page, "docker-known")
	if !strings.Contains(cmd, "-v "+secrets+":/secrets:ro \\") {
		t.Errorf("command mounts a different secrets directory than step 1 names (%q):\n%s", secrets, cmd)
	}
	if !strings.Contains(cmd, "src=/mnt/photos,dst=/source,readonly") {
		t.Errorf("command does not mount the client's source label /mnt/photos:\n%s", cmd)
	}
	for _, stale := range []string{"PATH/TO/BACKUP", "PATH/TO/SECRETS"} {
		if strings.Contains(page, stale) {
			t.Errorf("page still carries the %q placeholder, so the operator has two names for one path:\n%s", stale, page)
		}
	}
}

// TestCreatedPageUsesTheFormSecretsDirectory keeps the "one name" rule true when
// the operator typed a secrets directory of their own: step 1 names it and the
// command mounts it, rather than the /etc/backitup default.
func TestCreatedPageUsesTheFormSecretsDirectory(t *testing.T) {
	_, c, ts := pageStack(t, true)
	const secrets = "/srv/keys/nas-photos"
	page := createPage(t, c, ts, url.Values{
		"name": {"nas-photos"}, "mode": {"targz"},
		"source_label": {"/mnt/photos"}, "secrets_path": {secrets},
	})

	if !strings.Contains(page, "Create <code>"+secrets+"</code>") {
		t.Errorf("step 1 ignores the secrets directory from the form:\n%s", page)
	}
	for _, id := range []string{"docker-known", "docker-insecure"} {
		if cmd := fieldText(t, page, id); !strings.Contains(cmd, "-v "+secrets+":/secrets:ro \\") {
			t.Errorf("%s mounts a different directory than step 1 names (%q):\n%s", id, secrets, cmd)
		}
	}
}

// TestCreatedPageCronBoxIsPasteReady is defect 4: the cron step handed over a
// schedule and a comment. Each cron box must be one complete entry, free of
// comments, whose command is the one from the step above and which really
// reaches docker when a shell joins its continuation lines.
func TestCreatedPageCronBoxIsPasteReady(t *testing.T) {
	_, c, ts := pageStack(t, true)
	page := createPage(t, c, ts, url.Values{
		"name": {"nas-photos"}, "mode": {"targz"}, "source_label": {"/mnt/photos"},
	})
	token := pageToken(t, page)

	commands := map[string]string{
		"cron-known":    fieldText(t, page, "docker-known"),
		"cron-insecure": fieldText(t, page, "docker-insecure"),
	}
	for id, command := range commands {
		entry := fieldText(t, page, id)
		if entry == "" {
			t.Fatalf("%s is empty", id)
		}
		if strings.Contains(entry, "#") {
			t.Errorf("%s carries a '#', which a crontab reads as a comment:\n%s", id, entry)
		}
		if !strings.HasPrefix(entry, defaultCronSchedule+" docker run --rm \\") {
			t.Errorf("%s does not start with a schedule followed by the whole command:\n%s", id, entry)
		}
		// The same text the box shows, minus the schedule fields, is what cron
		// hands to the shell. Prove it reaches docker intact.
		args := runWithFakeDocker(t, strings.TrimPrefix(entry, defaultCronSchedule+" "))
		if !containsArg(args, "BACKITUP_TOKEN="+token) {
			t.Errorf("%s: docker never received the token from step 1 (argv=%q)", id, args)
		}
		if got := lastArg(args); got != "th0rn0/backitup-client:test" {
			t.Errorf("%s: last docker arg = %q, want the client image", id, got)
		}
		// The entry must be the command from the run step, not a second copy
		// that can drift from it.
		if !strings.HasSuffix(entry, command) {
			t.Errorf("%s does not carry the command from the run step verbatim:\nentry:\n%s\ncommand:\n%s", id, entry, command)
		}
	}
}

// TestCreatedPageWithoutHostKeyOffersNoVerifiedCommand is defect 3: with
// BACKITUP_SSH_HOST_KEY unset the page offered a Secure tab whose command
// mounted a known_hosts file the page never handed over. The page must now offer
// one command that works, say what it gives up, and say how to get a verified
// one.
func TestCreatedPageWithoutHostKeyOffersNoVerifiedCommand(t *testing.T) {
	_, c, ts := pageStack(t, false)
	page := createPage(t, c, ts, url.Values{
		"name": {"nas-photos"}, "mode": {"targz"}, "source_label": {"/mnt/photos"},
	})

	if strings.Contains(page, "BACKITUP_KNOWN_HOSTS") {
		t.Errorf("no host key, yet the page still renders a command referencing known_hosts:\n%s", page)
	}
	for _, id := range []string{"docker-known", "cron-known", "knownhosts"} {
		if textareaRE.MatchString(page) && strings.Contains(page, `id="`+id+`"`) {
			t.Errorf("page still renders box %q with no host key on the server", id)
		}
	}
	if !strings.Contains(page, `id="docker-insecure"`) {
		t.Errorf("page offers no runnable command at all:\n%s", page)
	}
	cmd := fieldText(t, page, "docker-insecure")
	if !strings.Contains(cmd, "-e BACKITUP_INSECURE=1 \\") {
		t.Errorf("the only command left does not carry the insecure flag:\n%s", cmd)
	}
	for _, want := range []string{
		"BACKITUP_SSH_HOST_KEY",
		"skips SSH host-key verification",
		"rotate this client",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page does not explain %q:\n%s", want, page)
		}
	}
}

// TestCreatedPageSourceLabelThatIsNotAPath covers the other half of defect 2:
// a label that describes the source ("photos on the NAS") is not a path, so the
// command carries a placeholder named for what it is and the page says to
// replace it. Nothing calls the source BACKUP.
func TestCreatedPageSourceLabelThatIsNotAPath(t *testing.T) {
	_, c, ts := pageStack(t, true)
	page := createPage(t, c, ts, url.Values{
		"name": {"nas-photos"}, "mode": {"targz"}, "source_label": {"photos on the NAS"},
	})

	cmd := fieldText(t, page, "docker-known")
	if !strings.Contains(cmd, "src="+sourcePlaceholder+",dst=/source,readonly") {
		t.Errorf("command does not carry the source placeholder %q:\n%s", sourcePlaceholder, cmd)
	}
	if !strings.Contains(page, "Replace <code>"+sourcePlaceholder+"</code>") {
		t.Errorf("page does not tell the operator to replace the source placeholder:\n%s", page)
	}
	if strings.Contains(page, "BACKUP") {
		t.Errorf("page still calls the source BACKUP:\n%s", page)
	}
}

// TestRotatePageIsNotTheCreateCopy is defects 5 and 6: rotation asked the
// operator to create a directory that already exists, never mentioned that the
// old token is already dead, and reissued known_hosts as though it had changed.
func TestRotatePageIsNotTheCreateCopy(t *testing.T) {
	_, c, ts := pageStack(t, true)
	createPage(t, c, ts, url.Values{
		"name": {"nas-photos"}, "mode": {"targz"}, "source_label": {"/mnt/photos"},
	})

	resp, page := mustPostForm(t, c, ts.URL+"/clients/nas-photos/rotate", url.Values{"confirm": {"1"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate = %d, want 200:\n%s", resp.StatusCode, truncate([]byte(page), 2000))
	}

	const secrets = "/etc/backitup/nas-photos"
	if strings.Contains(page, "Create <code>") {
		t.Errorf("rotation tells the operator to create a directory that already exists:\n%s", page)
	}
	if !strings.Contains(page, "The directory <code>"+secrets+"</code> already exists") {
		t.Errorf("rotation does not name the existing secrets directory %q:\n%s", secrets, page)
	}
	if !strings.Contains(page, "Overwrite <code>id</code>") {
		t.Errorf("rotation does not say to overwrite the existing id file:\n%s", page)
	}
	for _, want := range []string{
		"401 unauthorized",
		"invalid immediately",
		"known_hosts</code> is unchanged",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("rotation page does not say %q:\n%s", want, page)
		}
	}
	// The reissued page still has to be actionable: the new token, and a command
	// that mounts the same secrets directory and the client's own source.
	token := pageToken(t, page)
	cmd := fieldText(t, page, "cron-known")
	for _, want := range []string{
		"-e BACKITUP_TOKEN=" + token + " \\",
		"-v " + secrets + ":/secrets:ro \\",
		"src=/mnt/photos,dst=/source,readonly",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("rotated command is missing %q:\n%s", want, cmd)
		}
	}
}
