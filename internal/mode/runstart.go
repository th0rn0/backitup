package mode

import "time"

// RunStartNamed is implemented by a ServerMode that names each snapshot after
// the client run's own start time. rsync does: internal/mode/rsync formats
// snapshots/<ts> from the clock it read at the top of the run, so the snapshot
// name and the run row's started_at are the same instant.
//
// It exists so the server can attribute a snapshot on disk to the run row that
// created it, which is the only way to recognise the leftovers of a run that
// was killed before it could report. Such a run never reaches the end of its
// own code, so it never reports a snapshot id either: the name is the only link
// between the directory and the row that says the run never reached ok (see
// UnbackedSnapshots).
//
// A mode whose snapshot names are not the client's run start — tar.gz names its
// archive server-side, in backitup-recv, and renames it into place only on
// success, so a killed run leaves nothing under a snapshot name — does not
// implement this, and no snapshot of its is ever attributed to a run.
type RunStartNamed interface {
	// RunStart reports the run start time a snapshot's name encodes. ok is false
	// for a name that is not the client's run-start shape (a hand-placed entry,
	// an older layout): nothing can be said about what created it, so it must be
	// left alone.
	RunStart(s Snapshot) (start time.Time, ok bool)
}
