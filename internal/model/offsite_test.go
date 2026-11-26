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
	// A client that has chosen no prefix and is given no fleet default carries
	// its slug: that is what makes a file found in the bucket identifiable.
	client := func(mode Mode, uploadMode string) Client {
		return Client{Slug: "laptop", Mode: mode, OffsiteUploadMode: uploadMode}
	}

	tests := []struct {
		name       string
		client     Client
		fleet      string
		snapshotID string
		want       string
	}{
		// The immutable modes keep naming each object after its snapshot, with
		// the client's name in front of it.
		{"all/targz", client(ModeTarGz, OffsiteUploadAll), "", snap, "laptop-" + snap},
		{"latest/targz", client(ModeTarGz, OffsiteUploadLatest), "", snap, "laptop-" + snap},
		{"all/rsync", client(ModeRsync, OffsiteUploadAll), "", rsyncSnap, "laptop-" + rsyncSnap + ".tar.gz"},
		{"latest/rsync", client(ModeRsync, OffsiteUploadLatest), "", rsyncSnap, "laptop-" + rsyncSnap + ".tar.gz"},
		// Single generation writes one fixed name, whatever the snapshot or mode:
		// that is what makes each upload replace the last by overwrite. The
		// prefix goes in front of the fixed name, which stays fixed.
		{"single/targz", client(ModeTarGz, OffsiteUploadSingle), "", snap, "laptop-" + SingleObjectName},
		{"single/rsync", client(ModeRsync, OffsiteUploadSingle), "", rsyncSnap, "laptop-" + SingleObjectName},
		// An empty mode (a client stored before the column existed reads as "all")
		// must not resolve to the fixed name: that would make every upload
		// overwrite the same object for a client that never asked for it.
		{"empty mode/targz", client(ModeTarGz, ""), "", snap, "laptop-" + snap},
		{"empty mode/rsync", client(ModeRsync, ""), "", rsyncSnap, "laptop-" + rsyncSnap + ".tar.gz"},

		// The fleet default is what a client without its own prefix carries.
		{"fleet default", client(ModeTarGz, OffsiteUploadAll), "fleet", snap, "fleet-" + snap},
		// A client's own prefix wins over the fleet default.
		{"client prefix beats fleet", Client{Slug: "laptop", Mode: ModeTarGz, OffsiteUploadMode: OffsiteUploadAll,
			NamePrefix: "nas", NamePrefixSet: true}, "fleet", snap, "nas-" + snap},
		// "No prefix" is a choice, not an absence: it produces exactly the names
		// backitup wrote before prefixes existed (the regression this setting
		// must not break), and it beats the fleet default too.
		{"explicit no prefix", Client{Slug: "laptop", Mode: ModeTarGz, OffsiteUploadMode: OffsiteUploadAll,
			NamePrefixSet: true}, "fleet", snap, snap},
		{"explicit no prefix/single", Client{Slug: "laptop", Mode: ModeRsync, OffsiteUploadMode: OffsiteUploadSingle,
			NamePrefixSet: true}, "fleet", rsyncSnap, SingleObjectName},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := OffsiteObjectName(tc.client, tc.fleet, tc.snapshotID); got != tc.want {
				t.Fatalf("OffsiteObjectName(%+v, %q, %q) = %q, want %q",
					tc.client, tc.fleet, tc.snapshotID, got, tc.want)
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
	// The staging name carries the same prefix as the live name it replaces,
	// and stays in the same directory: it is where the upload lands, nothing
	// else.
	prefixed := Client{Slug: "laptop", Mode: ModeRsync, OffsiteUploadMode: OffsiteUploadSingle}
	if got, want := OffsiteObjectPartName(prefixed, ""), "laptop-"+SingleObjectPartName; got != want {
		t.Fatalf("OffsiteObjectPartName(prefixed) = %q, want %q", got, want)
	}
	unprefixed := prefixed
	unprefixed.NamePrefixSet = true
	if got := OffsiteObjectPartName(unprefixed, "fleet"); got != SingleObjectPartName {
		t.Fatalf("OffsiteObjectPartName(no prefix) = %q, want %q", got, SingleObjectPartName)
	}
}

// A client whose objects were written before name prefixes existed has them
// under the legacy spelling, so both spellings have to be addressable.
func TestOffsiteObjectNameCandidates(t *testing.T) {
	const snap = "backup-20261124T110000Z-000.tar.gz"

	prefixed := Client{Slug: "laptop", Mode: ModeTarGz, OffsiteUploadMode: OffsiteUploadAll}
	got := OffsiteObjectNameCandidates(prefixed, "", snap)
	if len(got) != 2 || got[0] != "laptop-"+snap || got[1] != snap {
		t.Fatalf("candidates = %v, want [laptop-%s %s]", got, snap, snap)
	}

	// With no prefix there is one name, and asking twice would mean listing the
	// remote for a client that cannot have two spellings.
	unprefixed := prefixed
	unprefixed.NamePrefixSet = true
	if got := OffsiteObjectNameCandidates(unprefixed, "fleet", snap); len(got) != 1 || got[0] != snap {
		t.Fatalf("candidates without a prefix = %v, want [%s]", got, snap)
	}

	// Single generation: the fixed name is the legacy spelling, and the prefixed
	// one still has to stay fixed across snapshots.
	single := Client{Slug: "laptop", Mode: ModeRsync, OffsiteUploadMode: OffsiteUploadSingle}
	got = OffsiteObjectNameCandidates(single, "", snap)
	if len(got) != 2 || got[0] != "laptop-"+SingleObjectName || got[1] != SingleObjectName {
		t.Fatalf("single candidates = %v, want [laptop-%s %s]", got, SingleObjectName, SingleObjectName)
	}
	if OffsiteObjectName(single, "", "20261125T110000Z") != got[0] {
		t.Fatalf("single-generation name changed with the snapshot: %q vs %q",
			OffsiteObjectName(single, "", "20261125T110000Z"), got[0])
	}
}
