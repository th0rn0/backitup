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
//
// It is also the legacy (unprefixed) spelling of that name, kept because a
// bucket written by an older backitup holds "latest.tar.gz" and verification
// must still see it.
const SingleObjectName = "latest.tar.gz"

// SingleObjectPartName is where an upload lands before the move that replaces
// the live object. Uploading straight to the live name would destroy the
// previous generation the moment the new one started, so a failed or truncated
// upload would leave the client with no cold copy at all.
const SingleObjectPartName = SingleObjectName + ".part"

// OffsiteObjectName returns the file name of the object that holds one
// snapshot's content inside the client's remote directory: the snapshot's own
// name for the immutable modes, the fixed name for OffsiteUploadSingle, and the
// client's resolved name prefix (model.NamePrefix) in front of either.
//
// This is the one place that rule lives, because three callers have to agree on
// it: the uploader (which writes the object), the verification pass (which lists
// the directory and asks whether the object is still there) and the server's
// download/delete routes (which address an object from a database row). If they
// disagreed, a record could name a file nobody wrote — the failure that made
// model.ObjectDir a single function too.
//
// The prefix decorates the NAME only. The directory is offsiteDir(c) in every
// case, so a prefix can never move an object (see InvalidNamePrefix).
//
// A prefixed single-generation client still has exactly one object and still
// overwrites it: the name is fixed at "<prefix>-latest.tar.gz" rather than
// varying per snapshot, which is the property that mode depends on.
func OffsiteObjectName(c Client, fleetDefault, snapshotID string) string {
	return prefixedName(NamePrefix(c, fleetDefault), objectShape(c.Mode, c.OffsiteUploadMode, snapshotID))
}

// objectShape is the object's name without any prefix: the snapshot's own name
// for the immutable modes, the fixed name for OffsiteUploadSingle. It is what
// the legacy spelling of an object is, so it is also what verification falls
// back to for a bucket written before prefixes existed.
func objectShape(mode Mode, uploadMode, snapshotID string) string {
	switch {
	case uploadMode == OffsiteUploadSingle:
		return SingleObjectName
	case mode == ModeRsync:
		// An rsync snapshot is a directory; it is uploaded as one archive.
		return snapshotID + ".tar.gz"
	}
	return snapshotID
}

// OffsiteObjectLegacyName is the name this client's object had before name
// prefixes existed — the same object without any prefix, whatever the client's
// setting says now. Verification accepts it so a bucket written by an older
// backitup does not read as data loss, and the download/delete routes fall back
// to it so the operator can still reach those objects.
func OffsiteObjectLegacyName(c Client, snapshotID string) string {
	return objectShape(c.Mode, c.OffsiteUploadMode, snapshotID)
}

// OffsiteObjectNameCandidates returns the names a client's object for
// snapshotID may have on the remote, newest spelling first: the current name
// and, when it differs, the legacy unprefixed one.
//
// One function answers "which names could this object have?" so the verification
// pass and the server's download/delete routes cannot disagree about what an
// existing bucket contains.
func OffsiteObjectNameCandidates(c Client, fleetDefault, snapshotID string) []string {
	current := OffsiteObjectName(c, fleetDefault, snapshotID)
	if legacy := OffsiteObjectLegacyName(c, snapshotID); legacy != current {
		return []string{current, legacy}
	}
	return []string{current}
}

// OffsiteObjectPartName is the staging name a single-generation upload writes
// before rclone moveto replaces the live object. It mirrors the live name, so a
// client can never leave a staging object under a name another client's prefix
// would also produce.
func OffsiteObjectPartName(c Client, fleetDefault string) string {
	return prefixedName(NamePrefix(c, fleetDefault), SingleObjectPartName)
}

// prefixedName puts prefix in front of name with the separator the object naming
// convention uses. An empty prefix returns name untouched, which is what makes
// "no prefix" produce exactly the names backitup wrote before it existed.
func prefixedName(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "-" + name
}
