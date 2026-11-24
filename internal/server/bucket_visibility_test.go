package server

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

// bucketAnswer is what the stub rclone says about the submitted key's access.
type bucketAnswer struct {
	// buckets is what `rclone lsd <remote>:` prints, one bucket per line.
	buckets []string
	// listErr, when set, makes that listing fail with this text on stderr: a
	// bucket-scoped B2 key has no listBuckets capability and answers 401.
	listErr string
}

// installBucketProbeRclone puts a stub `rclone` first on PATH. It records every
// invocation, answers `rclone lsd <remote>:` with the configured answer, and
// succeeds for `rclone lsd <remote>:<bucket>` — the probe the save-time check
// falls back to. Nothing here talks to B2.
func installBucketProbeRclone(t *testing.T, a bucketAnswer) (calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "rclone-calls.log")

	var listing strings.Builder
	if a.listErr != "" {
		listing.WriteString("    printf '%s\\n' " + shQuote(a.listErr) + " >&2\n")
		listing.WriteString("    exit 1\n")
	} else {
		for _, b := range a.buckets {
			listing.WriteString("    printf '   -1 2026-11-23 22:00:00        -1 %s\\n' " + shQuote(b) + "\n")
		}
	}
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + calls + "\n" +
		"last=\"\"\n" +
		"for a in \"$@\"; do last=\"$a\"; done\n" +
		// Only the account-root form (`<remote>:`) is answered from the fixture;
		// `<remote>:<bucket>` falls through to success.
		"case \"$last\" in\n" +
		"  *:)\n" + listing.String() + "    ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "rclone"), []byte(script), 0o755); err != nil {
		t.Fatalf("install fake rclone: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

// shQuote wraps s in single quotes for /bin/sh.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func readCalls(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read rclone calls: %v", err)
	}
	return string(b)
}

// A remote whose bucket the key cannot see must be refused at the form. rclone's
// B2 backend answers such a key by *creating* the bucket, so before this check
// the mistake only surfaced at upload time as `400 duplicate_bucket_name` on the
// destination path — "that name exists, and it is not yours to create".
func TestCreateRemoteRefusesBucketTheKeyCannotSee(t *testing.T) {
	st, ts, confPath := rcloneStackWithConf(t)
	c := loggedInClient(t, st, ts)
	calls := installBucketProbeRclone(t, bucketAnswer{
		buckets: []string{"tehhulk-source-archive", "thornotes-backups"},
	})

	body := postForm(t, c, ts.URL+"/settings/remotes", url.Values{
		"name":    {"tehHulk-source"},
		"account": {"004appkeyid"},
		"key":     {"K004applicationkey"},
		"bucket":  {"tehhulk-source"},
	})

	for _, want := range []string{
		`tehhulk-source`,         // the bucket that was refused, by name
		"not visible",            // why
		"tehhulk-source-archive", // what the key can see instead
		"thornotes-backups",
		"cannot be widened", // the fix is a new key, not an edit
		"new key",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, body[:min(len(body), 2000)])
		}
	}

	// Nothing was saved, and the credentials never reached the shared config.
	if r, err := st.GetRemoteByName(t.Context(), "tehHulk-source"); err != nil || r != nil {
		t.Errorf("a refused remote was stored anyway: remote=%v err=%v", r, err)
	}
	if conf, err := os.ReadFile(confPath); err == nil && strings.Contains(string(conf), "tehHulk-source") {
		t.Errorf("a refused remote was written to rclone.conf:\n%s", conf)
	}
	// The check probes with a throwaway config, never the shared one that every
	// upload reads.
	if log := readCalls(t, calls); strings.Contains(log, confPath) {
		t.Errorf("the bucket check ran against the shared rclone.conf:\n%s", log)
	}
}

// The positive control: the same form saves when the key can see the bucket.
func TestCreateRemoteAcceptsBucketTheKeyCanSee(t *testing.T) {
	st, ts, confPath := rcloneStackWithConf(t)
	c := loggedInClient(t, st, ts)
	installBucketProbeRclone(t, bucketAnswer{buckets: []string{"tehhulk-source"}})

	body := postForm(t, c, ts.URL+"/settings/remotes", url.Values{
		"name":    {"tehHulk-source"},
		"account": {"004appkeyid"},
		"key":     {"K004applicationkey"},
		"bucket":  {"tehhulk-source"},
	})
	if !strings.Contains(body, "created") {
		t.Fatalf("a remote whose bucket the key can see was not created:\n%s", body[:min(len(body), 2000)])
	}
	r, err := st.GetRemoteByName(t.Context(), "tehHulk-source")
	if err != nil || r == nil {
		t.Fatalf("remote not stored: remote=%v err=%v", r, err)
	}
	if conf, err := os.ReadFile(confPath); err != nil || !strings.Contains(string(conf), "[tehHulk-source]") {
		t.Fatalf("rclone.conf does not carry the new section (err=%v):\n%s", err, conf)
	}
}

// A key scoped to one bucket is the key a remote should have, and B2 gives such
// a key no listBuckets capability: `rclone lsd <remote>:` answers 401. The check
// must accept that configuration, so it falls back to asking about the bucket
// itself — listing the bucket is what proves the key can address it.
func TestCreateRemoteAcceptsBucketScopedKey(t *testing.T) {
	st, ts, _ := rcloneStackWithConf(t)
	c := loggedInClient(t, st, ts)
	calls := installBucketProbeRclone(t, bucketAnswer{
		listErr: "ERROR : error listing: Unknown 401  (401 unauthorized)",
	})

	body := postForm(t, c, ts.URL+"/settings/remotes", url.Values{
		"name":    {"tehHulk-source"},
		"account": {"004appkeyid"},
		"key":     {"K004applicationkey"},
		"bucket":  {"tehhulk-source"},
	})
	if !strings.Contains(body, "created") {
		t.Fatalf("a bucket-scoped key was refused because it cannot enumerate buckets:\n%s", body[:min(len(body), 2000)])
	}

	log := readCalls(t, calls)
	for _, want := range []string{
		"lsd tehHulk-source:",               // it did ask what the key can see
		"lsd tehHulk-source:tehhulk-source", // then asked about the bucket itself
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the bucket check never ran %q; calls:\n%s", want, log)
		}
	}
}

// An edit is checked the same way, and a refused edit leaves the stored remote
// as it was: silently repointing it at a bucket the key cannot see is how the
// upload starts failing three passes later.
func TestEditRemoteRefusesBucketTheKeyCannotSee(t *testing.T) {
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	if err := st.CreateRemote(t.Context(), model.Remote{
		Name: "tehHulk-source", Backend: model.BackendB2,
		Config: map[string]string{
			"account": "004appkeyid", "key": "K004applicationkey", "bucket": "old-bucket",
		},
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed remote: %v", err)
	}
	installBucketProbeRclone(t, bucketAnswer{buckets: []string{"old-bucket", "other-bucket"}})

	body := postForm(t, c, ts.URL+"/settings/remotes/tehHulk-source/edit", url.Values{
		"bucket": {"tehhulk-source"},
	})
	for _, want := range []string{"not visible", "tehhulk-source", "old-bucket"} {
		if !strings.Contains(body, want) {
			t.Errorf("the edit refusal does not say %q:\n%s", want, body[:min(len(body), 2000)])
		}
	}

	r, err := st.GetRemoteByName(t.Context(), "tehHulk-source")
	if err != nil || r == nil {
		t.Fatalf("load remote: remote=%v err=%v", r, err)
	}
	if r.Config["bucket"] != "old-bucket" {
		t.Errorf("a refused edit changed the stored bucket to %q", r.Config["bucket"])
	}
}
