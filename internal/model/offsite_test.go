package model

import "testing"

// The object name is the agreement between three callers: the uploader writes
// it, the verification pass lists the directory and asks whether it is still
// there, and the server's download route addresses it from a row. A mistake here
// means a record names a file nobody wrote, or a download asks for a file that
// was never uploaded.
func TestOffsiteObjectName(t *testing.T) {
	const snap = "backup-20261124T110000Z-000.tar.gz"
	const rsyncSnap = "20261124T110000Z"

	tests := []struct {
		name       string
		mode       Mode
		uploadMode string
		snapshotID string
		want       string
	}{
		// The immutable modes keep naming each object after its snapshot.
		{"all/targz", ModeTarGz, OffsiteUploadAll, snap, snap},
		{"latest/targz", ModeTarGz, OffsiteUploadLatest, snap, snap},
		{"all/rsync", ModeRsync, OffsiteUploadAll, rsyncSnap, rsyncSnap + ".tar.gz"},
		{"latest/rsync", ModeRsync, OffsiteUploadLatest, rsyncSnap, rsyncSnap + ".tar.gz"},
		// Single generation writes one fixed name, whatever the snapshot or mode:
		// that is what makes each upload replace the last by overwrite.
		{"single/targz", ModeTarGz, OffsiteUploadSingle, snap, SingleObjectName},
		{"single/rsync", ModeRsync, OffsiteUploadSingle, rsyncSnap, SingleObjectName},
		// An empty mode (a client stored before the column existed reads as "all")
		// must not resolve to the fixed name: that would make every upload
		// overwrite the same object for a client that never asked for it.
		{"empty mode/targz", ModeTarGz, "", snap, snap},
		{"empty mode/rsync", ModeRsync, "", rsyncSnap, rsyncSnap + ".tar.gz"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := OffsiteObjectName(tc.mode, tc.uploadMode, tc.snapshotID); got != tc.want {
				t.Fatalf("OffsiteObjectName(%q, %q, %q) = %q, want %q",
					tc.mode, tc.uploadMode, tc.snapshotID, got, tc.want)
			}
		})
	}
}

// The staging name is derived from the live one, so a rename of the object
// cannot leave the upload writing a name nothing moves.
func TestSingleObjectPartNameIsTheStagingName(t *testing.T) {
	if SingleObjectPartName != SingleObjectName+".part" {
		t.Fatalf("SingleObjectPartName = %q, want %q", SingleObjectPartName, SingleObjectName+".part")
	}
}
