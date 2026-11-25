package rsync

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/mode"
)

// wrapperPath is the forced command this repository installs into the sshd
// image: the thing authorized_keys forces for a client that records ownership.
// Paths from a package test are relative to the package directory.
func wrapperPath(t *testing.T) string {
	t.Helper()
	// Absolute: the ssh stand-in cd's into the jail before exec'ing it.
	abs, err := filepath.Abs(filepath.Join("..", "..", "..", "docker", "sshd", "backitup-rrsync"))
	if err != nil {
		t.Fatalf("resolve the wrapper's path: %v", err)
	}
	return abs
}

// runWrapper executes the shipped forced command with SSH_ORIGINAL_COMMAND set
// and a stand-in `rrsync` on PATH that just prints the command line it was
// handed, so the injection rules can be asserted without rsync or a transfer.
func runWrapper(t *testing.T, original string) string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is needed to run the forced-command wrapper")
	}
	binDir := t.TempDir()
	fake := "#!/bin/sh\nprintf '%s' \"$SSH_ORIGINAL_COMMAND\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "rrsync"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, wrapperPath(t), "/srv/backups/client")
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"), "SSH_ORIGINAL_COMMAND="+original)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper %q: %v\n%s", original, err, out)
	}
	return string(out)
}

// With the setting off the run log is byte-for-byte what it was before the
// setting existed: an operator who has not opted in must not be told about
// xattrs in their snapshots.
func TestOwnershipNoteOnlyWhenTheSettingIsOn(t *testing.T) {
	if got := ownershipNote(mode.BackupOpts{}); got != "" {
		t.Fatalf("ownershipNote with the setting off = %q, want no line", got)
	}
	note := ownershipNote(mode.BackupOpts{RecordOwnership: true})
	if note == "" {
		t.Fatal("the setting on produced no log line")
	}
	// The line has to name the three things an operator needs: that the run
	// recorded ownership, where it lives, and how not to lose it.
	for _, want := range []string{"uid/gid", "user.rsync.%stat", "cp"} {
		if !strings.Contains(note, want) {
			t.Errorf("the log line does not mention %q: %s", want, note)
		}
	}
}

// An xattr failure on the receiver is exit 23 with a line per entry; without this
// the operator sees "some files/attrs were not transferred" and has nothing to
// act on. The text below is VERBATIM output from the ingest image (rsync 3.4.1)
// pushing through backitup-rrsync into a jail on a vfat loop mount, which is the
// volume type that refuses user xattrs.
func TestXattrFailureHintNamesTheSettingAndTheVolume(t *testing.T) {
	const refused = `rsync: [generator] failed to write xattr user.rsync.%stat for "/proc/self/fd/4/vfat-on/.": Operation not supported (95)
rsync: [receiver] failed to write xattr user.rsync.%stat for "/proc/self/fd/4/vfat-on/.owned.txt.1MDSe0": Operation not supported (95)
rsync error: some files/attrs were not transferred (see previous errors) (code 23) at main.c(1356) [sender=3.2.7]
`
	hint := xattrFailureHint(mode.BackupOpts{RecordOwnership: true}, refused)
	if hint == "" {
		t.Fatal("a refused xattr produced no hint: the operator gets rsync's exit 23 and nothing else")
	}
	for _, want := range []string{"Record source ownership", "extended attributes", "Operation not supported", "ext4", "NFS"} {
		if !strings.Contains(hint, want) {
			t.Errorf("the hint does not mention %q: %s", want, hint)
		}
	}
	// The same text must be recognised when the setting is off too: rsync wrote
	// the pseudostat, so it is still this mechanism that failed.
	if hint := xattrFailureHint(mode.BackupOpts{}, refused); !strings.Contains(hint, "Record source ownership") {
		t.Errorf("the measured text is not attributed to the ownership setting when the flag is off: %q", hint)
	}
}

// The older 3.x spelling, and that the setting is chosen from the attribute name
// rather than from whichever flag happens to be on.
func TestXattrFailureHintFromTheOlderSpelling(t *testing.T) {
	const refused = `rsync: [receiver] rsync_xal_set: lsetxattr("snapshots/x/f","user.notes") failed: Operation not supported (95)`
	hint := xattrFailureHint(mode.BackupOpts{PreserveXattrs: true}, refused)
	if strings.Contains(hint, "Record source ownership") {
		t.Fatalf("the hint blamed the ownership setting for a -X failure: %s", hint)
	}
	if !strings.Contains(hint, "Preserve extended attributes") {
		t.Fatalf("the hint does not name the setting that asked for the xattr: %s", hint)
	}

	acl := xattrFailureHint(mode.BackupOpts{PreserveACLs: true},
		`rsync: [receiver] set_xattr_acl: lsetxattr("snapshots/x/f","system.posix_acl_access") failed: Operation not supported (95)`)
	if !strings.Contains(acl, "Preserve ACLs") {
		t.Fatalf("an ACL write failure was not attributed to the ACL setting: %s", acl)
	}
}

// A run that failed for an unrelated reason must not be explained by xattrs.
func TestXattrFailureHintIgnoresOtherFailures(t *testing.T) {
	for _, out := range []string{
		"rsync: [sender] send_files failed to open \"/src/locked.qcow2\": Permission denied (13)",
		"rsync error: some files/attrs were not transferred (see previous errors) (code 23)",
		"rsync: connection unexpectedly closed (0 bytes received so far) [sender]",
	} {
		if hint := xattrFailureHint(mode.BackupOpts{RecordOwnership: true}, out); hint != "" {
			t.Errorf("a non-xattr failure got an xattr hint %q from:\n%s", hint, out)
		}
	}
}

// A permission failure (EACCES rather than EOPNOTSUPP) is a different sentence:
// the volume does support xattrs, the account could not write one, and telling
// the operator to move the store would be wrong advice.
func TestXattrFailureHintDistinguishesPermissionFromUnsupported(t *testing.T) {
	const denied = `rsync: [receiver] rsync_xal_set: lsetxattr("snapshots/x/f","user.rsync.%stat") failed: Permission denied (13)`
	hint := xattrFailureHint(mode.BackupOpts{RecordOwnership: true}, denied)
	if hint == "" {
		t.Fatal("a denied xattr write produced no hint")
	}
	if strings.Contains(hint, "move BACKITUP_BACKUP_DIR") {
		t.Fatalf("a permission failure was explained as an unsupported volume: %s", hint)
	}
	if !strings.Contains(hint, "Permission denied") {
		t.Fatalf("the hint dropped the receiver's own reason: %s", hint)
	}
}

// The flag's position in the forced command is load-bearing twice over: rrsync
// wants --server to be the second word, and it reads the direction from --sender
// being the first word after it. Inserting the flag before --sender would turn a
// restore into a write, which is why this is asserted instead of assumed.
func TestOwnershipWrapperInjectsAfterServerOrAfterSender(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			name: "receive (a backup)",
			in:   "rsync --server -logDtpre.iLsfxCIvu . snapshots/ts/",
			want: "rsync --server --fake-super -logDtpre.iLsfxCIvu . snapshots/ts/",
		},
		{
			name: "send (a restore)",
			in:   "rsync --server --sender -logDtpre.iLsfxCIvu . snapshots/ts/",
			want: "rsync --server --sender --fake-super -logDtpre.iLsfxCIvu . snapshots/ts/",
		},
		{
			name: "already asked for",
			in:   "rsync --server --fake-super -logDtpre.iLsfxCIvu . snapshots/ts/",
			want: "rsync --server --fake-super -logDtpre.iLsfxCIvu . snapshots/ts/",
		},
		{
			name: "not rsync at all",
			in:   "true",
			want: "true",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runWrapper(t, tc.in)
			if got != tc.want {
				t.Fatalf("wrapper turned\n  %q\ninto\n  %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}
