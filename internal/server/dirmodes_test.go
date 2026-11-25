package server

import (
	"bytes"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/th0rn0/backitup/internal/store"
)

// The hot store is read by an operator whose account is in the backup user's
// group (gid 10001 in the shipped image) — never as root. Two directories per
// client are what stand between that account and the data: <clientDir> and, for
// rsync, <clientDir>/snapshots. They are 0750 now; everything below them is the
// rsync receiver's business (see model.Client.GroupReadable).

func mustMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

func mustOwner(t *testing.T, path string) (uid, gid uint32) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skipf("no stat uid/gid on this platform (%T)", fi.Sys())
	}
	return st.Uid, st.Gid
}

// The acceptance case: a freshly created rsync client has both of its
// server-owned directories at 0750, belonging to the server's own uid/gid — in
// production that is uid 10001, gid 10001, because that is what the server runs
// as. The test asserts the property (the server's own uid) rather than the
// literal, which a developer box cannot arrange.
func TestNewClientDirsAreGroupTraversable(t *testing.T) {
	st, srv, ts := hardeningStack(t)
	c := loggedInClient(t, st, ts)
	postForm(t, c, ts.URL+"/clients", url.Values{"name": {"docs"}, "mode": {"rsync"}})

	base := filepath.Join(srv.backupBaseDir, "docs")
	for _, dir := range []string{base, filepath.Join(base, "snapshots")} {
		if got := mustMode(t, dir); got != ClientDirMode {
			t.Errorf("%s has mode %04o, want %04o", dir, got, ClientDirMode)
		}
		uid, gid := mustOwner(t, dir)
		if uid != uint32(os.Geteuid()) || gid != uint32(os.Getegid()) {
			t.Errorf("%s is owned %d:%d, want the server's own %d:%d", dir, uid, gid, os.Geteuid(), os.Getegid())
		}
	}
}

// A targz client has one server-owned directory (no snapshots/ subdirectory, the
// receiver mkdirs as it needs), and the group gets in there too.
func TestTarGzClientDirIsGroupTraversable(t *testing.T) {
	st, srv, ts := hardeningStack(t)
	c := loggedInClient(t, st, ts)
	postForm(t, c, ts.URL+"/clients", url.Values{"name": {"archive"}, "mode": {"targz"}})

	base := filepath.Join(srv.backupBaseDir, "archive")
	if got := mustMode(t, base); got != ClientDirMode {
		t.Errorf("%s has mode %04o, want %04o", base, got, ClientDirMode)
	}
}

// The app-data directory — the SQLite database — is not part of the hot store and
// stays private. Widening it would expose every client's token hash and history
// for no operator benefit.
func TestAppDataDirStaysPrivate(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "appdata")
	st, err := store.Open(filepath.Join(dataDir, "backitup.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if got := mustMode(t, dataDir); got != 0o700 {
		t.Errorf("app-data directory has mode %04o, want 0700", got)
	}
	// The directory is the guard: the database file itself is whatever SQLite
	// creates (subject to the umask), and nothing outside the directory can reach
	// it. Widening the directory would expose every token hash and every client's
	// history, so it is the one that must not move.
}

// A store written by a build that predates ClientDirMode has 0700 on both
// directories, and the operator's new group membership buys nothing until they
// are healed. One reconcile pass — what startup runs — is enough, and it says so
// once per directory.
func TestReconcileWidensDirectoriesAnOlderBuildCreated(t *testing.T) {
	st, srv, ts := hardeningStack(t)
	c := loggedInClient(t, st, ts)
	postForm(t, c, ts.URL+"/clients", url.Values{"name": {"docs"}, "mode": {"rsync"}})

	base := filepath.Join(srv.backupBaseDir, "docs")
	snaps := filepath.Join(base, "snapshots")
	for _, dir := range []string{base, snaps} {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatalf("pretend to be the previous build: chmod %s: %v", dir, err)
		}
	}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	srv.ReconcileClientDirModes(t.Context())

	for _, dir := range []string{base, snaps} {
		if got := mustMode(t, dir); got != ClientDirMode {
			t.Errorf("after one reconcile %s has mode %04o, want %04o", dir, got, ClientDirMode)
		}
	}
	if !strings.Contains(buf.String(), `client "docs"`) {
		t.Errorf("the heal was silent; log says:\n%s", buf.String())
	}
	// Once per directory, not once per pass: a second pass on a store that is now
	// correct says nothing at all.
	buf.Reset()
	srv.ReconcileClientDirModes(t.Context())
	if s := buf.String(); strings.Contains(s, base) {
		t.Errorf("a second pass repeated itself about %s:\n%s", base, s)
	}
}

// A directory owned by another uid is the bind-mount misconfiguration, and it is
// left exactly as it is with one line naming the client. Silently chmod-ing a
// tree somebody else owns is worse than a legible failure.
//
// selfUID is passed in because the test process cannot chown: a uid that is not
// the process's own is the condition under test, and it is reachable without
// privileges this way.
func TestForeignOwnedClientDirIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	selfUID := uint32(os.Geteuid()) + 1

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	if err := reconcileDirMode(dir, "docs", selfUID); err != nil {
		t.Fatalf("reconcileDirMode on a foreign directory: %v", err)
	}
	if got := mustMode(t, dir); got != 0o700 {
		t.Errorf("a directory owned by another uid was chmod-ed: mode %04o, want 0700", got)
	}
	s := buf.String()
	if !strings.Contains(s, `client "docs"`) || !strings.Contains(s, "owned by uid") {
		t.Errorf("the refusal does not name the client and the owner:\n%s", s)
	}
	// One line per directory.
	buf.Reset()
	if err := reconcileDirMode(dir, "docs", selfUID); err != nil {
		t.Fatalf("second reconcileDirMode: %v", err)
	}
	if again := buf.String(); strings.Contains(again, dir) {
		t.Errorf("the same directory was reported twice:\n%s", again)
	}
}

// A directory that is already group-traversable is left with the mode it has:
// this heals a store that is too private, it does not normalise one that is not.
func TestReconcileDoesNotNarrowAWiderDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := reconcileDirMode(dir, "docs", uint32(os.Geteuid())); err != nil {
		t.Fatalf("reconcileDirMode: %v", err)
	}
	if got := mustMode(t, dir); got != 0o775 {
		t.Errorf("mode %04o, want the untouched 0775", got)
	}
}
