package model

// The values clients.offsite_upload_mode can take.
//
// They are separate values rather than options of one setting because each
// answers a different question about what cold storage holds, and changing what
// an existing value means would silently change the storage behaviour of every
// client already on it.
const (
	// OffsiteUploadAll mirrors every snapshot as its own immutable object. The
	// bucket's lifecycle rule is the only thing that ever removes one.
	OffsiteUploadAll = "all"
	// OffsiteUploadLatest uploads at most one un-offsited snapshot per pass, but
	// every upload is still its own object: the earlier ones stay until the
	// bucket rule expires them, so this bounds work, not storage.
	OffsiteUploadLatest = "latest"
	// OffsiteUploadSingle keeps exactly one object per client, at a fixed name,
	// replaced by every successful upload. It is the only mode that bounds what
	// cold storage holds without backitup deleting an offsite object — which it
	// no longer does, on any schedule.
	OffsiteUploadSingle = "single"
)

// SingleObjectName is the fixed object name an OffsiteUploadSingle client
// writes. The snapshot id is deliberately absent from it: the name has to stay
// the same across uploads for each one to replace the last by overwrite.
const SingleObjectName = "latest.tar.gz"

// SingleObjectPartName is where an upload lands before the move that replaces
// the live object. Uploading straight to SingleObjectName would destroy the
// previous generation the moment the new one started, so a failed or truncated
// upload would leave the client with no cold copy at all.
const SingleObjectPartName = SingleObjectName + ".part"

// OffsiteObjectName returns the file name of the object that holds one
// snapshot's content inside the client's remote directory: the snapshot's own
// name for the immutable modes, the fixed name for OffsiteUploadSingle.
//
// This is the one place that rule lives, because three callers have to agree on
// it: the uploader (which writes the object), the verification pass (which lists
// the directory and asks whether the object is still there) and the server's
// download/delete routes (which address an object from a database row). If they
// disagreed, a record could name a file nobody wrote — the failure that made
// model.ObjectDir a single function too.
func OffsiteObjectName(mode Mode, uploadMode, snapshotID string) string {
	if uploadMode == OffsiteUploadSingle {
		return SingleObjectName
	}
	if mode == ModeRsync {
		// An rsync snapshot is a directory; it is uploaded as one archive.
		return snapshotID + ".tar.gz"
	}
	return snapshotID
}
