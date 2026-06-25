// Package rsync implements the rsync client backup mode: rsnapshot-style
// hardlinked snapshots over SSH, confined by the server's rrsync forced command.
//
// Recipe (verified against the ingest container's rrsync jail):
//   - snapshot:  rsync -a --delete --link-dest=/snapshots/latest src/ host:snapshots/<ts>/
//     The link-dest MUST be anchored at the jail root (/snapshots/latest);
//     rrsync rejects "../latest" outright.
//   - flip latest: rsync a local "latest -> <ts>" symlink to host:snapshots/.
//
// Unchanged files hardlink to the previous snapshot (cheap incrementals); the
// source is only ever READ.
package rsync

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/th0rn0/backitup/internal/mode"
	"github.com/th0rn0/backitup/internal/model"
)

// Mode is the rsync client mode.
type Mode struct{}

func (Mode) Mode() model.Mode { return model.ModeRsync }

func init() { mode.RegisterClient(Mode{}) }

// Backup syncs SourceDir into a new timestamped snapshot dir on the server,
// hardlinking unchanged files against the previous snapshot, then flips latest.
func (Mode) Backup(ctx context.Context, o mode.BackupOpts) (mode.BackupResult, error) {
	logger := o.Log()
	start := time.Now().UTC()
	if _, err := exec.LookPath("rsync"); err != nil {
		return mode.BackupResult{}, fmt.Errorf("rsync not found: %w", err)
	}
	host, sshArgs, err := sshTransport(o)
	if err != nil {
		return mode.BackupResult{}, err
	}
	snap := start.Format("20060102T150405Z")
	target := fmt.Sprintf("%s@%s:snapshots/%s/", o.SSHUser, host, snap)

	args := []string{"-a", "--delete", "--stats", "--link-dest=/snapshots/latest"}
	if os.Getenv("BACKITUP_RSYNC_DEBUG") == "1" {
		args = append(args, "-vvv")
	}
	if o.SkipSymlinks {
		args = append(args, "--no-links")
	}
	for _, ex := range o.Excludes {
		args = append(args, "--exclude="+ex)
	}
	args = append(args, "-e", sshArgs, ensureTrailingSlash(o.SourceDir), target)

	logger.Printf("syncing %s → %s", o.SourceDir, target)
	stdout, err := runRsync(ctx, logger, args)
	if err != nil {
		return mode.BackupResult{}, err
	}

	if err := flipLatest(ctx, o, host, sshArgs, snap); err != nil {
		return mode.BackupResult{}, fmt.Errorf("update latest pointer: %w", err)
	}

	files, written := parseStats(stdout)
	logger.Printf("synced %d files, %s", files, mode.HumanBytes(written))
	return mode.BackupResult{
		SnapshotID: snap,
		Bytes:      written,
		Files:      files,
		StartedAt:  start,
		FinishedAt: time.Now().UTC(),
	}, nil
}

// flipLatest points snapshots/latest at the new snapshot by rsyncing a local
// symlink (rrsync forbids server-side commands and "..").
func flipLatest(ctx context.Context, o mode.BackupOpts, host, sshArgs, snap string) error {
	stage, err := os.MkdirTemp("", "backitup-latest-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	link := filepath.Join(stage, "latest")
	if err := os.Symlink(snap, link); err != nil {
		return err
	}
	target := fmt.Sprintf("%s@%s:snapshots/", o.SSHUser, host)
	_, err = runRsync(ctx, o.Log(), []string{"-a", "-e", sshArgs, link, target})
	return err
}

// lineLogger is an io.Writer that logs each complete line in real time.
// stdout and stderr each get their own instance so there is no shared state
// across the two goroutines that exec.Cmd creates to drain those pipes.
type lineLogger struct {
	logger *log.Logger
	buf    []byte
}

func (l *lineLogger) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		if line := strings.TrimRight(string(l.buf[:i]), "\r"); line != "" {
			l.logger.Print(line)
		}
		l.buf = l.buf[i+1:]
	}
	return len(p), nil
}

func (l *lineLogger) flush() {
	if line := strings.TrimRight(string(l.buf), "\r\n "); line != "" {
		l.logger.Print(line)
	}
}

func runRsync(ctx context.Context, logger *log.Logger, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, "rsync", args...)
	var outBuf, errBuf bytes.Buffer
	outLL := &lineLogger{logger: logger}
	errLL := &lineLogger{logger: logger}
	cmd.Stdout = io.MultiWriter(&outBuf, outLL)
	cmd.Stderr = io.MultiWriter(&errBuf, errLL)
	err := cmd.Run()
	outLL.flush()
	errLL.flush()
	if err != nil {
		return outBuf.String(), fmt.Errorf("rsync: %w: %s", err, strings.TrimSpace(errBuf.String()))
	}
	out := outBuf.String()
	if se := strings.TrimSpace(errBuf.String()); se != "" {
		out += "\n" + se
	}
	return out, nil
}

// sshTransport returns the bare host and the rsync "-e" ssh command string,
// wiring the client key and host-key policy.
// Set BACKITUP_SSH_DEBUG=1 to add -vvv to the SSH command for SSH-layer diagnostics.
// Set BACKITUP_RSYNC_DEBUG=1 to add -vvv to the rsync command for rsync-protocol diagnostics.
func sshTransport(o mode.BackupOpts) (host, sshArgs string, err error) {
	host, port, err := net.SplitHostPort(o.SSHServer)
	if err != nil {
		return "", "", fmt.Errorf("bad SSHServer %q: %w", o.SSHServer, err)
	}
	parts := []string{
		"ssh", "-i", o.SSHKey, "-p", port,
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=30",
		"-o", "ServerAliveInterval=60",
		"-o", "ServerAliveCountMax=3",
	}
	if os.Getenv("BACKITUP_SSH_DEBUG") == "1" {
		parts = append(parts, "-vvv")
	}
	if o.InsecureSSH {
		parts = append(parts, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null")
	} else {
		if o.KnownHosts == "" {
			return "", "", fmt.Errorf("host-key verification requires a known_hosts file (or set insecure)")
		}
		parts = append(parts, "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+o.KnownHosts)
	}
	return host, strings.Join(parts, " "), nil
}

func ensureTrailingSlash(p string) string {
	if strings.HasSuffix(p, "/") {
		return p
	}
	return p + "/"
}

var (
	// Use snapshot totals (not just transferred-only) so that runs where
	// --link-dest hardlinks everything still show the full snapshot size on
	// the dashboard rather than "0 files, 0 B".
	reFiles = regexp.MustCompile(`Number of files:\s*([\d,]+)`)
	reBytes = regexp.MustCompile(`Total file size:\s*([\d,]+)`)
)

// parseStats best-effort extracts files/bytes from rsync --stats output. A miss
// returns 0 rather than failing the backup.
func parseStats(out string) (files, bytesN int64) {
	if m := reFiles.FindStringSubmatch(out); m != nil {
		files = atoiCommas(m[1])
	}
	if m := reBytes.FindStringSubmatch(out); m != nil {
		bytesN = atoiCommas(m[1])
	}
	return files, bytesN
}

func atoiCommas(s string) int64 {
	n, _ := strconv.ParseInt(strings.ReplaceAll(s, ",", ""), 10, 64)
	return n
}
