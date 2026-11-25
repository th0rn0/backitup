package targz

// The client half of the tar.gz mode had no test at all: `Backup` dials sshd,
// hands it the archive on stdin and composes the run's error from what the
// remote said. That composition is the code an operator reads when a backup
// fails, so it is exercised here against a real SSH session — an in-process
// sshd on 127.0.0.1 (nothing leaves the machine) running a handler that stands
// in for the forced command `backitup-recv`.
//
// The archive the client streams is then opened and extracted the way a restore
// would, because "the upload succeeded" is not the claim that matters: the
// bytes on the far side have to be the tree that went in.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/th0rn0/backitup/internal/keys"
	"github.com/th0rn0/backitup/internal/mode"
)

// ── an in-process ingest sshd ────────────────────────────────────────────────

// ingest is a single-connection-at-a-time sshd listening on loopback. It
// answers exactly what a tar.gz session needs: one "session" channel and an
// "exec" request, which it either accepts (and then hands the open channel to
// the test's handler, as sshd hands stdin to the forced command) or refuses.
type ingest struct {
	addr string
	cmds chan string // the command the client asked for, for the record
	mu   sync.Mutex
	err  error // what the handler hit; asserted by the test, never by t.Fatalf in a goroutine
}

func (i *ingest) handlerErr() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.err
}

func (i *ingest) fail(err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.err == nil {
		i.err = err
	}
}

// startIngest starts the fake sshd. rejectExec makes it refuse the client's
// "exec" request, which is what a server whose forced command is gone does.
func startIngest(t *testing.T, rejectExec bool, h func(ch ssh.Channel, cmd string)) *ingest {
	t.Helper()

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatalf("host signer: %v", err)
	}
	cfg := &ssh.ServerConfig{
		// The client authenticates with the key backitup issued it; the harness
		// cares about the session, not about which key was used.
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	in := &ingest{addr: ln.Addr().String(), cmds: make(chan string, 4)}
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return // listener closed at cleanup
			}
			go in.serve(nc, cfg, rejectExec, h)
		}
	}()
	return in
}

func (i *ingest) serve(nc net.Conn, cfg *ssh.ServerConfig, rejectExec bool, h func(ssh.Channel, string)) {
	defer nc.Close() //nolint:errcheck
	sconn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		i.fail(fmt.Errorf("server handshake: %w", err))
		return
	}
	defer sconn.Close() //nolint:errcheck
	go ssh.DiscardRequests(reqs)

	for nch := range chans {
		if nch.ChannelType() != "session" {
			_ = nch.Reject(ssh.UnknownChannelType, "only session channels")
			continue
		}
		ch, chReqs, err := nch.Accept()
		if err != nil {
			i.fail(fmt.Errorf("accept channel: %w", err))
			continue
		}
		go func(ch ssh.Channel, chReqs <-chan *ssh.Request) {
			for req := range chReqs {
				if req.Type != "exec" {
					_ = req.Reply(false, nil)
					continue
				}
				cmd := strings.TrimSpace(string(req.Payload[4:])) // uint32 length prefix
				if rejectExec {
					_ = req.Reply(false, nil)
					_ = ch.Close()
					return
				}
				_ = req.Reply(true, nil)
				i.cmds <- cmd
				h(ch, cmd)
				return // the handler owns the channel from here
			}
		}(ch, chReqs)
	}
}

// exitStatus ends a session the way sshd does: the child's status first, then
// the channel close that unblocks the client's Wait.
func exitStatus(ch ssh.Channel, code uint32, stderr string) {
	if stderr != "" {
		_, _ = io.WriteString(ch.Stderr(), stderr)
	}
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
	_ = ch.Close()
}

// ── helpers ─────────────────────────────────────────────────────────────────

// clientKey writes a fresh private key for the client and returns its path.
func clientKey(t *testing.T) string {
	t.Helper()
	priv, _, err := keys.GenerateKeypair("backitup:test")
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	p := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(p, []byte(priv), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return p
}

// sourceTree builds the tree a backup captures and returns its directory.
// n files so the progress hook (called every 1000 files) is exercised too.
func sourceTree(t *testing.T, n int) (dir string, files map[string]string) {
	t.Helper()
	dir = t.TempDir()
	files = make(map[string]string, n)
	for i := 0; i < n; i++ {
		name := filepath.Join(dir, "docs", fmt.Sprintf("file-%04d.txt", i))
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		body := fmt.Sprintf("contents of file %d\n", i)
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		files["docs/"+filepath.Base(name)] = body
	}
	return dir, files
}

// backupOpts wires a client at the harness.
func backupOpts(t *testing.T, in *ingest, src string) mode.BackupOpts {
	t.Helper()
	return mode.BackupOpts{
		SourceDir:   src,
		SSHServer:   in.addr,
		SSHUser:     "backitup",
		SSHKey:      clientKey(t),
		InsecureSSH: true, // the harness host key changes every run; that is the point of a throwaway sshd
	}
}

// extractArchive reads a .tar.gz the way `tar -xzf` does and returns its
// regular files, path -> contents.
func extractArchive(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer f.Close() //nolint:errcheck
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("archive is not gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Includes the CRC error a truncated or corrupted archive leaves.
			t.Fatalf("read archive: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s from archive: %v", hdr.Name, err)
		}
		out[hdr.Name] = string(body)
	}
	return out
}

// ── the tests ───────────────────────────────────────────────────────────────

// The happy path end to end: every byte the client read from the source reaches
// the server, and what landed there extracts back to the same tree. The run
// reports the file count and the uncompressed size, which is what the dashboard
// shows as "this backup holds N files".
func TestBackupStreamsAnExtractableArchive(t *testing.T) {
	dir := t.TempDir()
	src, want := sourceTree(t, 1000) // 1000 files: the progress hook fires exactly once

	var logs strings.Builder
	in := startIngest(t, false, func(ch ssh.Channel, _ string) {
		f, err := os.Create(filepath.Join(dir, "backup-20260101T000000Z-000.tar.gz"))
		if err != nil {
			_ = ch.Close()
			return
		}
		// Read to EOF: the client closes its stdin when the walk is done.
		if _, err := io.Copy(f, ch); err != nil {
			_ = f.Close()
			_ = ch.Close()
			return
		}
		if err := f.Close(); err != nil {
			_ = ch.Close()
			return
		}
		exitStatus(ch, 0, "")
	})

	opts := backupOpts(t, in, src)
	opts.Logger = log.New(&logs, "", 0)

	res, err := Mode{}.Backup(context.Background(), opts)
	if err != nil {
		t.Fatalf("backup: %v (logs: %s)", err, logs.String())
	}
	if herr := in.handlerErr(); herr != nil {
		t.Fatalf("the far side of the session failed: %v", herr)
	}

	if res.Files != int64(len(want)) {
		t.Errorf("run reports %d files, the source has %d", res.Files, len(want))
	}
	if res.Bytes == 0 {
		t.Error("run reports 0 bytes for a non-empty tree")
	}
	if res.FinishedAt.Before(res.StartedAt) {
		t.Errorf("finished %s before started %s", res.FinishedAt, res.StartedAt)
	}
	// The tar.gz receiver names the snapshot server-side, so the client cannot
	// know it — an empty id here is the real shape of a tar.gz report.
	if res.SnapshotID != "" {
		t.Errorf("tar.gz Backup reported snapshot id %q; recv picks that name, so any check keyed on it would fail every real tar.gz backup", res.SnapshotID)
	}

	// What the server holds, extracted the way a restore would read it.
	got := extractArchive(t, filepath.Join(dir, "backup-20260101T000000Z-000.tar.gz"))
	if len(got) != len(want) {
		t.Fatalf("archive holds %d files, source had %d", len(got), len(want))
	}
	var names []string
	for name := range want {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if got[name] != want[name] {
			t.Errorf("%s in the archive = %q, source has %q", name, got[name], want[name])
		}
	}

	// The run log says what it did, and the progress hook reported mid-walk.
	logsText := logs.String()
	for _, wantLine := range []string{
		"connecting to " + in.addr,
		"archiving " + src,
		"archiving... 1000 files", // progress, not just the final count
		"archived 1000 files",
	} {
		if !strings.Contains(logsText, wantLine) {
			t.Errorf("run log is missing %q; log:\n%s", wantLine, logsText)
		}
	}
	// The command the client sends is ignored by the forced command, but it is
	// still what it sends: a change here would mean the client and the sshd
	// configuration disagree about the protocol. The harness hands the command
	// back through a buffered channel before the handler runs, so it is already
	// there by the time the session ends — no waiting.
	select {
	case cmd := <-in.cmds:
		if cmd != "backup" {
			t.Errorf("client asked for command %q, want \"backup\"", cmd)
		}
	default:
		t.Error("the harness recorded no command request at all")
	}
}

// The refusal that matters most: backitup-recv aborts an upload over its byte
// cap before the archive is complete, so the client's next write fails and the
// only local error is a broken pipe. The operator must see the server's own
// sentence — it names the limit — not "broken pipe".
func TestBackupReportsTheReceiversRefusal(t *testing.T) {
	const refusal = "backitup-recv: upload exceeds BACKITUP_RECV_MAX_BYTES (1048576 bytes); aborted after 1048577 bytes, partial archive removed, no snapshot written"

	in := startIngest(t, false, func(ch ssh.Channel, _ string) {
		// Refuse without reading: exit non-zero and say why, as an aborted
		// upload does.
		exitStatus(ch, 1, refusal+"\n")
	})

	src, _ := sourceTree(t, 5)
	var logs strings.Builder
	opts := backupOpts(t, in, src)
	opts.Logger = log.New(&logs, "", 0)

	res, err := Mode{}.Backup(context.Background(), opts)
	if err == nil {
		t.Fatalf("a refused upload reported success: %+v", res)
	}
	if !strings.Contains(err.Error(), "BACKITUP_RECV_MAX_BYTES (1048576 bytes)") {
		t.Errorf("the reported error does not carry the server's reason: %v", err)
	}
	if res.Files != 0 || res.Bytes != 0 {
		t.Errorf("a failed run returned counts (%d files, %d bytes); the report must show nothing was stored", res.Files, res.Bytes)
	}
}

// A server that takes the whole stream and only then fails (a rename or fsync
// that did not work) leaves no local error at all, so the failure has to come
// from the session's exit status and still quote what the server said.
func TestBackupReportsASessionFailureAfterTheStream(t *testing.T) {
	in := startIngest(t, false, func(ch ssh.Channel, _ string) {
		_, _ = io.Copy(io.Discard, ch) // accept every byte
		exitStatus(ch, 1, "backitup-recv: writing the upload to /srv/backups/x/….part failed (out of space?); partial archive removed\n")
	})

	src, _ := sourceTree(t, 3)
	res, err := Mode{}.Backup(context.Background(), backupOpts(t, in, src))
	if err == nil {
		t.Fatalf("a failed remote upload reported success: %+v", res)
	}
	if !strings.Contains(err.Error(), "remote upload failed") {
		t.Errorf("error = %v, want the remote-failure wording", err)
	}
	if !strings.Contains(err.Error(), "out of space") {
		t.Errorf("the server's own message is missing from %v", err)
	}
}

// A failed dial must not be reported as a failed backup of the files: the run
// never started. The error names the address, because that is the setting the
// operator has to fix.
func TestBackupDialErrors(t *testing.T) {
	src, _ := sourceTree(t, 1)

	t.Run("unusable key", func(t *testing.T) {
		opts := mode.BackupOpts{SourceDir: src, SSHServer: "127.0.0.1:1", SSHUser: "backitup",
			SSHKey: filepath.Join(t.TempDir(), "no-such-key"), InsecureSSH: true}
		if _, err := (Mode{}).Backup(context.Background(), opts); err == nil {
			t.Fatal("a missing client key was accepted")
		}
	})

	t.Run("no host-key verification", func(t *testing.T) {
		// Secure mode with no known_hosts fails closed: dial must refuse rather
		// than trust whatever answers.
		opts := mode.BackupOpts{SourceDir: src, SSHServer: "127.0.0.1:1", SSHUser: "backitup",
			SSHKey: clientKey(t), KnownHosts: "", InsecureSSH: false}
		_, err := (Mode{}).Backup(context.Background(), opts)
		if err == nil {
			t.Fatal("dial without known_hosts and without insecure mode was accepted")
		}
		if !strings.Contains(err.Error(), "known_hosts") {
			t.Errorf("error = %v, want it to name known_hosts", err)
		}
	})

	t.Run("nothing listening", func(t *testing.T) {
		// A closed port on loopback: the dial itself fails.
		opts := mode.BackupOpts{SourceDir: src, SSHServer: unusedAddr(t), SSHUser: "backitup",
			SSHKey: clientKey(t), InsecureSSH: true}

		_, err := (Mode{}).Backup(context.Background(), opts)
		if err == nil {
			t.Fatal("dial to a closed port succeeded")
		}
		if !strings.Contains(err.Error(), "ssh dial") {
			t.Errorf("error = %v, want an ssh dial failure", err)
		}
	})
}

// A server that refuses the exec request kills the run before a byte moves.
// The error must say the remote never started, not that the archive failed.
func TestBackupFailsWhenTheRemoteWillNotStart(t *testing.T) {
	in := startIngest(t, true, func(ch ssh.Channel, _ string) { exitStatus(ch, 0, "") })
	src, _ := sourceTree(t, 2)

	_, err := Mode{}.Backup(context.Background(), backupOpts(t, in, src))
	if err == nil {
		t.Fatal("a refused exec request was reported as a completed backup")
	}
	if !strings.Contains(err.Error(), "start remote") {
		t.Errorf("error = %v, want it to say the remote never started", err)
	}
}

// An unreadable source is a local failure the server knows nothing about, so the
// error is the archive's own — and the session still has to be closed cleanly.
// The test process is not root, so 0000 really is EACCES.
func TestBackupFailsOnAnUnreadableSource(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions; the EACCES this asserts cannot happen")
	}
	in := startIngest(t, false, func(ch ssh.Channel, _ string) {
		_, _ = io.Copy(io.Discard, ch)
		exitStatus(ch, 0, "")
	})

	src := t.TempDir()
	closed := filepath.Join(src, "sealed")
	if err := os.Mkdir(closed, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(closed, "secret"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(closed, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(closed, 0o700) })

	_, err := Mode{}.Backup(context.Background(), backupOpts(t, in, src))
	if err == nil {
		t.Fatal("a source with an unreadable directory was archived as if complete")
	}
	if !strings.Contains(err.Error(), "sealed") {
		t.Errorf("error = %v, want it to name the directory it could not read", err)
	}
}

// unusedAddr returns a loopback address nothing is listening on: bind, learn the
// port, release it.
func unusedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}
