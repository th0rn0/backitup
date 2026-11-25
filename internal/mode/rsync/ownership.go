package rsync

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/th0rn0/backitup/internal/mode"
)

// Ownership recording is the one fidelity property the CLIENT cannot ask for.
//
// Every other setting in fidelity.go becomes an rsync flag on the client's own
// command. This one cannot: authorized_keys locks the client's key to a forced
// command (`restrict,command="backitup-rrsync <jail>"`), so a client-supplied
// --rsync-path is ignored and the server's options come from that command
// string. A client with the setting on talks to a wrapper that adds --fake-super
// to the rsync running on the server, and the unprivileged receiver records the
// source's uid/gid in a `user.rsync.%stat` xattr instead of chowning.
//
// So all the client does here is (a) say in the run log what the snapshot it is
// about to take will hold, and (b) turn a receiver that cannot store xattrs into
// an actionable sentence instead of rsync's exit 23.
const ownershipLogLine = "ownership: the server records the source's uid/gid (receiver-side --fake-super, `user.rsync.%stat`) — " +
	"read it back with `rsync --fake-super` on the sending side; a plain `cp` out of the snapshot does not carry it"

// ownershipNote is the run-log line for a client that records ownership, or ""
// when it does not (so a run with the setting off logs exactly what it logged
// before the setting existed).
func ownershipNote(o mode.BackupOpts) string {
	if !o.RecordOwnership {
		return ""
	}
	return ownershipLogLine
}

// reWriteXattr matches the refusal rsync 3.4.1 (what the ingest image ships)
// actually prints when it cannot store an attribute. Measured on a vfat loop
// mount through a real SSH hop, for a client with the ownership setting on:
//
//	rsync: [generator] failed to write xattr user.rsync.%stat for "/proc/self/fd/4/vfat-on/.": Operation not supported (95)
//	rsync: [receiver] failed to write xattr user.rsync.%stat for ".../.owned.txt.1MDSe0": Operation not supported (95)
//	rsync error: some files/attrs were not transferred (see previous errors) (code 23) at main.c(1356)
//
// The attribute NAME is captured because that is what decides which setting to
// blame: this one message serves --fake-super, -X and the ACL store alike.
var reWriteXattr = regexp.MustCompile(`failed to write xattr ([^\s]+) for "[^"]*"(?::\s*([^\n]*))?`)

// reLsetxattr matches the older spelling some 3.x builds emit for -X and ACLs:
//
//	rsync_xal_set: lsetxattr(<path>,"user.foo") failed: Operation not supported (95)
//	set_xattr_acl: lsetxattr(<path>,"system.posix_acl_access") failed: Permission denied (13)
var reLsetxattr = regexp.MustCompile(`(?:rsync_xal_set|set_xattr_acl): lsetxattr\("[^"]*","([^"]*)"\) failed(?::\s*([^\n]*))?`)

// xattrFailure is one parsed refusal: the attribute rsync could not store and the
// reason the receiver gave.
type xattrFailure struct {
	attr   string
	reason string
}

// parseXattrFailure returns the first refusal in output, or ok=false.
func parseXattrFailure(output string) (xattrFailure, bool) {
	if m := reWriteXattr.FindStringSubmatch(output); m != nil {
		return xattrFailure{attr: m[1], reason: strings.TrimSpace(m[2])}, true
	}
	if m := reLsetxattr.FindStringSubmatch(output); m != nil {
		return xattrFailure{attr: m[1], reason: strings.TrimSpace(m[2])}, true
	}
	return xattrFailure{}, false
}

// xattrFailureHint turns a receiver's xattr failure into an operator-actionable
// sentence, naming the setting that asked for it and the volume that refused it.
// "" means the run failed for some other reason.
//
// The setting is chosen from the attribute NAME, not from which flag this client
// happens to have on: `user.rsync.%stat` is what --fake-super writes, `user.*`
// comes from -X, `system.posix_acl*` from -A. Blaming the wrong one sends the
// operator to change a setting that was not involved.
func xattrFailureHint(o mode.BackupOpts, output string) string {
	f, ok := parseXattrFailure(output)
	if !ok {
		return ""
	}
	// ENOTSUP/EOPNOTSUPP both read "Operation not supported"; so do the
	// volumes that accept user xattrs on regular files but refuse them on
	// directories or symlinks, which is a real difference between ext4 and a
	// network filesystem.
	unsupported := strings.Contains(strings.ToLower(f.reason), "not supported")

	var setting string
	switch {
	case f.attr == "user.rsync.%stat":
		setting = "the \"Record source ownership\" setting could not store the source's ownership in an xattr"
		if unsupported {
			setting = "the \"Record source ownership\" setting needs user extended attributes (xattrs) on the backups volume, " +
				"which this one refuses"
		}
	case strings.HasPrefix(f.attr, "system.posix_acl"):
		setting = "the \"Preserve ACLs\" setting could not store the ACL"
		if unsupported {
			setting = "the \"Preserve ACLs\" setting needs xattrs on the backups volume, which this one refuses"
		}
	case o.PreserveXattrs && unsupported:
		setting = "the \"Preserve extended attributes\" setting needs user xattrs on the backups volume, which this one refuses"
	default:
		setting = "the receiving side could not store the extended attribute " + f.attr
	}

	if unsupported {
		return fmt.Sprintf("%s (%s): ext4, xfs and btrfs support them, an NFS or fat volume may not — "+
			"move BACKITUP_BACKUP_DIR to a supporting volume, or turn the setting off for this client", setting, f.reason)
	}
	return fmt.Sprintf("%s (%s) — the backup is not complete; the run is marked failed", setting, f.reason)
}
