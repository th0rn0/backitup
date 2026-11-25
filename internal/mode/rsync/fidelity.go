package rsync

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/th0rn0/backitup/internal/mode"
)

// The fidelity settings are per-client, opt-in, and each one is exactly one
// rsync flag. This table is the single place that knows the flag, the wording
// the client page uses, the form field that sets it, and the read of
// mode.BackupOpts — so a run's log, a refused option and the UI cannot drift
// apart.
//
// Every one defaults to off: a client created before the settings existed keeps
// producing exactly the snapshot it always did (see the regression test in
// fidelity_test.go). -S and -H decide what the snapshot HOLDS. -A and -X carry
// metadata the receiving side can only apply as the file's owner — which it is,
// because it creates the file, so a plain non-root receiver sets them fine
// (measured: see the PR body).
type fidelitySetting struct {
	flag  string // the rsync flag, as it appears in a run log and an error
	label string // what the snapshot carries, in the client page's words
	field string // the form field on the client page
	set   func(mode.BackupOpts) bool
}

var fidelitySettings = []fidelitySetting{
	{"-S", "sparse files (a hole stays a hole)", "preserve_sparse", func(o mode.BackupOpts) bool { return o.PreserveSparse }},
	{"-H", "internal hardlinks (one inode, not one copy per name)", "preserve_hardlinks", func(o mode.BackupOpts) bool { return o.PreserveHardlinks }},
	{"-A", "POSIX ACLs", "preserve_acls", func(o mode.BackupOpts) bool { return o.PreserveACLs }},
	{"-X", "extended attributes", "preserve_xattrs", func(o mode.BackupOpts) bool { return o.PreserveXattrs }},
}

// fidelityArgs returns the flags this client's settings ask for, in table
// order. A client with every setting off gets nothing, which is what keeps the
// 86de926 invocation byte for byte.
func fidelityArgs(o mode.BackupOpts) []string {
	var args []string
	for _, s := range fidelitySettings {
		if s.set(o) {
			args = append(args, s.flag)
		}
	}
	return args
}

// fidelityFlags is the same set as a readable list for the run log ("" when the
// client asked for nothing).
func fidelityFlags(o mode.BackupOpts) string {
	return strings.Join(fidelityArgs(o), " ")
}

// reRefusedOption matches rrsync's refusal of an option it does not allow. The
// script prints "<path> error: option -A has been disabled on this server." to
// stderr and exits 1; rsync then reports a protocol failure and the transfer
// never starts. (Measured against rrsync 3.5.0 in the shipped recv image.)
var reRefusedOption = regexp.MustCompile(`option\s+(-[A-Za-z]+)\s+has been disabled on this server`)

// refusedFidelityOption turns a receiver's refusal of one of our flags into an
// operator-actionable sentence. Without it the run's log says
//
//	rsync: connection unexpectedly closed (0 bytes received so far) [sender]
//	rsync error: error in rsync protocol data stream (code 12)
//
// which names no flag and no setting, and the operator's only clue is that the
// backup stopped working.
func refusedFidelityOption(output string) string {
	m := reRefusedOption.FindStringSubmatch(output)
	if m == nil {
		return ""
	}
	flag, said := m[1], strings.TrimSpace(m[0])
	for _, s := range fidelitySettings {
		if s.flag == flag {
			return fmt.Sprintf("the receiving side refused %s (%s): turn off the %q setting for this client — it said %q",
				flag, s.label, s.field, said)
		}
	}
	return fmt.Sprintf("the receiving side refused %s: turn off the fidelity setting that asks for it — it said %q", flag, said)
}
