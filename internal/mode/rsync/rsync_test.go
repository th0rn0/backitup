package rsync

import (
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/mode"
)

func TestParseStats(t *testing.T) {
	out := `
Number of files: 10 (reg: 8, dir: 2)
Number of regular files transferred: 3
Total file size: 4,096 bytes
Total transferred file size: 1,234 bytes
`
	files, bytesN := parseStats(out)
	// snapshot totals (10 files, 4096 bytes), not transferred-only (3, 1234)
	if files != 10 {
		t.Errorf("files = %d, want 10", files)
	}
	if bytesN != 4096 {
		t.Errorf("bytes = %d, want 4096", bytesN)
	}
}

func TestParseStatsMissing(t *testing.T) {
	files, bytesN := parseStats("no stats here")
	if files != 0 || bytesN != 0 {
		t.Fatalf("missing stats should be 0/0, got %d/%d", files, bytesN)
	}
}

func TestSSHTransportInsecure(t *testing.T) {
	host, args, err := sshTransport(mode.BackupOpts{SSHServer: "h:2222", SSHKey: "/k", Insecure: true})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if host != "h" {
		t.Errorf("host = %q, want h", host)
	}
	for _, want := range []string{"ssh", "-i /k", "-p 2222", "StrictHostKeyChecking=no"} {
		if !strings.Contains(args, want) {
			t.Errorf("ssh args %q missing %q", args, want)
		}
	}
}

func TestSSHTransportKnownHosts(t *testing.T) {
	_, args, err := sshTransport(mode.BackupOpts{SSHServer: "h:22", SSHKey: "/k", KnownHosts: "/kh"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(args, "StrictHostKeyChecking=yes") || !strings.Contains(args, "UserKnownHostsFile=/kh") {
		t.Errorf("expected strict host checking against /kh, got %q", args)
	}
}

func TestSSHTransportRequiresVerification(t *testing.T) {
	// Neither known_hosts nor insecure -> refuse (fail closed).
	if _, _, err := sshTransport(mode.BackupOpts{SSHServer: "h:22", SSHKey: "/k"}); err == nil {
		t.Fatal("expected error when neither known_hosts nor insecure is set")
	}
}

func TestSSHTransportBadServer(t *testing.T) {
	if _, _, err := sshTransport(mode.BackupOpts{SSHServer: "no-port", SSHKey: "/k", Insecure: true}); err == nil {
		t.Fatal("expected error for host without port")
	}
}

func TestEnsureTrailingSlash(t *testing.T) {
	if ensureTrailingSlash("/a") != "/a/" || ensureTrailingSlash("/a/") != "/a/" {
		t.Fatal("ensureTrailingSlash wrong")
	}
}

func TestModeName(t *testing.T) {
	if (Mode{}).Mode() != "rsync" {
		t.Fatalf("mode = %q", Mode{}.Mode())
	}
}
