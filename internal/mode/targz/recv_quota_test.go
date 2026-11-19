package targz

// These tests drive docker/sshd/backitup-recv — the forced command sshd runs
// for every tar.gz upload, and the only thing standing between one client's
// archive and the shared backup volume. It is a shell script, so it is
// exercised as a subprocess exactly as sshd runs it: /bin/sh, the client's
// archive on stdin and a temp directory standing in for the client's own
// store. A guard that only exists in a script nobody runs in CI is not a
// guard.
//
// Both BACKITUP_RECV_* variables are always set here: the script prefers them
// over /etc/backitup/recv-limits (which the container writes), so the tests are
// deterministic on a host that happens to carry that file.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	// Paths are relative to this package's directory (internal/mode/targz).
	recvScript       = "../../../docker/sshd/backitup-recv"
	recvLimitsScript = "../../../docker/sshd/recv-limits.sh"
	// The path entrypoint.sh hands the limits to and backitup-recv reads. Both
	// are asserted below: the file is written by one script and sourced by the
	// other, so a rename in either place silently disables the guard.
	recvLimitsPath = "/etc/backitup/recv-limits"
)

// recvResult is what a client sees of a forced command: an exit status (which
// its report carries to the dashboard) and whatever it said on stderr (which
// ends up in the run's log tail).
type recvResult struct {
	code   int
	stderr string
}

// runRecv streams data to backitup-recv for dir, as sshd would. data is staged
// in a file and handed over as an *os.File so a script that stops reading early
// (an over-cap abort) cannot turn into an EPIPE on our side of the pipe.
func runRecv(t *testing.T, dir string, env []string, data []byte) recvResult {
	t.Helper()
	return runRecvScript(t, recvScript, dir, env, data)
}

// runRecvScript is runRecv for a specific copy of the script.
func runRecvScript(t *testing.T, script, dir string, env []string, data []byte) recvResult {
	t.Helper()

	src := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open payload: %v", err)
	}
	defer in.Close()

	cmd := exec.Command("/bin/sh", script, dir)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = in
	var stderr strings.Builder
	cmd.Stderr = &stderr

	res := recvResult{}
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run backitup-recv: %v", err)
		}
		res.code = exit.ExitCode()
	}
	res.stderr = stderr.String()
	return res
}

// payload returns n bytes with no long repeats, so a truncated, duplicated or
// reordered write cannot compare equal to what was sent.
func payload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + i/251)
	}
	return b
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) { // nothing was created at all
			return nil
		}
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// An upload over the cap must fail the run (non-zero exit, named limit) and
// leave nothing behind: not the archive, and not the .part file an interrupted
// upload leaves — that is what eats the volume the guards exist to protect.
func TestRecvAbortsUploadOverByteCap(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "client")

	res := runRecv(t, dir,
		[]string{"BACKITUP_RECV_MAX_BYTES=1048576", "BACKITUP_RECV_MIN_FREE_BYTES=0"}, // 1 MiB
		payload(3<<20)) // 3 MiB, three times the cap

	if res.code == 0 {
		t.Fatalf("an upload over the cap succeeded; stderr: %q", res.stderr)
	}
	if !strings.Contains(res.stderr, "BACKITUP_RECV_MAX_BYTES (1048576 bytes)") {
		t.Fatalf("stderr must name the limit that was hit, got %q", res.stderr)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("partial archive left behind: %v (stderr: %q)", names, res.stderr)
	}
}

// An upload under the cap is still a backup: same exit status, and the archive
// holds every byte that was sent.
func TestRecvWritesCompleteArchiveUnderCap(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "client")
	want := payload(3 << 20)

	res := runRecv(t, dir,
		[]string{"BACKITUP_RECV_MAX_BYTES=4194304", "BACKITUP_RECV_MIN_FREE_BYTES=0"}, // 4 MiB
		want)
	if res.code != 0 {
		t.Fatalf("upload under the cap failed with %d: %q", res.code, res.stderr)
	}

	names := dirNames(t, dir)
	if len(names) != 1 {
		t.Fatalf("want exactly one archive, got %v", names)
	}
	// The name shape the rest of the app depends on (mode.ValidateSnapshotID,
	// the lifecycle's .part pruning and the status corroboration window).
	if !regexp.MustCompile(`^backup-\d{8}T\d{6}Z-\d{3}\.tar\.gz$`).MatchString(names[0]) {
		t.Fatalf("archive name %q is not backup-<ts>-<seq>.tar.gz", names[0])
	}
	got, err := os.ReadFile(filepath.Join(dir, names[0]))
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("archive is %d bytes, sent %d — truncation is a corrupt snapshot", len(got), len(want))
	}
	if string(got) != string(want) {
		t.Fatal("archive contents differ from what was streamed")
	}
}

// A volume without the reserved headroom must refuse the run instead of filling
// the last bytes and failing halfway through. The floor is set above any real
// volume so the test does not have to control the free space of the temp dir.
func TestRecvRefusesWhenFreeSpaceBelowFloor(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "client")
	const floor = "4611686018427387904" // 4 EiB: no volume the test runs on

	res := runRecv(t, dir,
		[]string{"BACKITUP_RECV_MAX_BYTES=53687091200", "BACKITUP_RECV_MIN_FREE_BYTES=" + floor},
		payload(1<<10))

	if res.code == 0 {
		t.Fatalf("a run with no headroom succeeded; stderr: %q", res.stderr)
	}
	if !strings.Contains(res.stderr, "BACKITUP_RECV_MIN_FREE_BYTES ("+floor+")") {
		t.Fatalf("stderr must name the floor that was not met, got %q", res.stderr)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("refused run wrote %v", names)
	}
}

// A limit nobody can parse must stop the run, not silently disarm it: the
// failure mode of a guard that reads as "disabled" is a full volume.
func TestRecvRefusesUnparsableLimit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "client")

	res := runRecv(t, dir,
		[]string{"BACKITUP_RECV_MAX_BYTES=50GiB", "BACKITUP_RECV_MIN_FREE_BYTES=0"},
		payload(1<<10))

	if res.code == 0 {
		t.Fatalf("an unparsable limit was accepted; stderr: %q", res.stderr)
	}
	if !strings.Contains(res.stderr, "BACKITUP_RECV_MAX_BYTES must be a whole number of bytes") {
		t.Fatalf("stderr must say which variable is wrong, got %q", res.stderr)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("run with an unparsable limit wrote %v", names)
	}
}

// The container's environment is what an operator edits, but sshd does not pass
// it to a session, so entrypoint.sh converts it into the file backitup-recv
// sources. That conversion is the link the guards depend on, so it is covered
// here too.
func TestRecvLimitsFileFromContainerEnvironment(t *testing.T) {
	out := filepath.Join(t.TempDir(), "recv-limits")

	cmd := exec.Command("/bin/sh", recvLimitsScript, out)
	cmd.Env = append(os.Environ(),
		"BACKITUP_RECV_MAX_BYTES=1234",
		"BACKITUP_RECV_MIN_FREE_BYTES=5678",
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("recv-limits.sh: %v (%s)", err, stderr.String())
	}

	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read limits file: %v", err)
	}
	// Assignment lines only: backitup-recv sources this file.
	want := "BACKITUP_RECV_MAX_BYTES=1234\nBACKITUP_RECV_MIN_FREE_BYTES=5678\n"
	if string(body) != want {
		t.Fatalf("limits file = %q, want %q", body, want)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat limits file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o444 {
		t.Fatalf("limits file mode = %v, want 0444 (the sshd user must not edit its own limits)", perm)
	}

	// An unset variable leaves the script's own default in place rather than
	// writing an empty assignment.
	cmd = exec.Command("/bin/sh", recvLimitsScript, out)
	cmd.Env = append(os.Environ(), "BACKITUP_RECV_MIN_FREE_BYTES=5678")
	if err := cmd.Run(); err != nil {
		t.Fatalf("recv-limits.sh (one variable): %v", err)
	}
	body, err = os.ReadFile(out)
	if err != nil {
		t.Fatalf("read limits file: %v", err)
	}
	if string(body) != "BACKITUP_RECV_MIN_FREE_BYTES=5678\n" {
		t.Fatalf("limits file = %q, want only the variable that was set", body)
	}

	// A value a shell must not interpolate is dropped with a warning, and the
	// run falls back to the default instead of sourcing something else.
	cmd = exec.Command("/bin/sh", recvLimitsScript, out)
	cmd.Env = append(os.Environ(), "BACKITUP_RECV_MAX_BYTES=$(id)", "BACKITUP_RECV_MIN_FREE_BYTES=5678")
	stderr.Reset()
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("recv-limits.sh (bad value): %v", err)
	}
	body, err = os.ReadFile(out)
	if err != nil {
		t.Fatalf("read limits file: %v", err)
	}
	if strings.Contains(string(body), "$(") || strings.Contains(string(body), "id)") {
		t.Fatalf("limits file carries an unvalidated value: %q", body)
	}
	if !strings.Contains(stderr.String(), "ignoring BACKITUP_RECV_MAX_BYTES") {
		t.Fatalf("a dropped value must be reported, stderr = %q", stderr.String())
	}
}

// The two ends of the file hand-off must name the same path: entrypoint.sh
// writes /etc/backitup/recv-limits and backitup-recv sources it. This is a text
// assertion rather than a functional one because the path is absolute and the
// test cannot own /etc — it still catches a rename on either side, which is the
// realistic way the guards would stop being applied.
func TestRecvLimitsPathAgreesAtBothEnds(t *testing.T) {
	for _, f := range []string{recvScript, "../../../docker/sshd/entrypoint.sh"} {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if !strings.Contains(string(body), recvLimitsPath) {
			t.Fatalf("%s does not mention %s", f, recvLimitsPath)
		}
	}
}

// The session environment carries no BACKITUP_RECV_* at all — that is what sshd
// gives a forced command, and it is why the limits are handed over in a file.
// This runs a copy of the script whose only change is the file path (the
// absolute one is not writable from a test) and shows the limits arriving from
// the file: a 1 KiB cap aborts a 4 KiB upload. A variable set in the session
// must still win, since that is what lets an operator run the script by hand.
func TestRecvReadsLimitsFromContainerFile(t *testing.T) {
	body, err := os.ReadFile(recvScript)
	if err != nil {
		t.Fatalf("read %s: %v", recvScript, err)
	}
	dir := t.TempDir()
	conf := filepath.Join(dir, "recv-limits")
	if err := os.WriteFile(conf, []byte("BACKITUP_RECV_MAX_BYTES=1024\nBACKITUP_RECV_MIN_FREE_BYTES=0\n"), 0o600); err != nil {
		t.Fatalf("write limits file: %v", err)
	}
	patched := strings.Replace(string(body), recvLimitsPath, conf, 1)
	if patched == string(body) {
		t.Fatalf("%s no longer reads %s", recvScript, recvLimitsPath)
	}
	script := filepath.Join(dir, "backitup-recv")
	if err := os.WriteFile(script, []byte(patched), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}

	// Only PATH survives: nothing named BACKITUP_* is in this environment, as in
	// an sshd session.
	sessionEnv := []string{"PATH=" + os.Getenv("PATH")}
	client := filepath.Join(dir, "client")

	res := runRecvScript(t, script, client, sessionEnv, payload(4096))
	if res.code == 0 {
		t.Fatalf("the limits file was not applied; stderr: %q", res.stderr)
	}
	if !strings.Contains(res.stderr, "BACKITUP_RECV_MAX_BYTES (1024 bytes)") {
		t.Fatalf("stderr must name the limit taken from the file, got %q", res.stderr)
	}

	// The environment still overrides the file: 4 KiB under an 8 KiB cap passes.
	res = runRecvScript(t, script, filepath.Join(dir, "client2"),
		append(sessionEnv, "BACKITUP_RECV_MAX_BYTES=8192"), payload(4096))
	if res.code != 0 {
		t.Fatalf("an overridden higher limit was ignored: %d (%q)", res.code, res.stderr)
	}
}
