package server

import (
	"strings"
	"testing"
)

// These are the unit tests for the rule that stops rclone.conf regeneration
// from destroying an operator's own sections. The API-level proof is
// TestHandWrittenRcloneSectionSurvivesEveryWrite; this file pins the merge
// itself, including the migration from files written by an older backitup.

// managedBlock renders the block mergeRcloneConfig is expected to produce.
func managedBlock(sections ...string) string {
	var sb strings.Builder
	sb.WriteString(rcloneManagedBegin + "\n\n")
	for _, s := range sections {
		sb.WriteString(strings.TrimRight(s, "\n") + "\n\n")
	}
	sb.WriteString(rcloneManagedEnd + "\n")
	return sb.String()
}

func TestMergeRcloneConfigPreservesOperatorContent(t *testing.T) {
	const handWritten = "# operator notes\n[crypt]\ntype = crypt\nremote = provider:backed"
	const s3Section = "[s3]\ntype = s3\n"

	cases := []struct {
		name      string
		existing  string
		generated []string
		want      string
	}{
		{
			name:      "new file: just the managed block",
			existing:  "",
			generated: []string{s3Section},
			want:      managedBlock(s3Section),
		},
		{
			name:      "hand-written section is kept ahead of the block",
			existing:  handWritten + "\n",
			generated: []string{s3Section},
			want:      handWritten + "\n\n" + managedBlock(s3Section),
		},
		{
			name:      "last run's block is replaced, not appended to",
			existing:  managedBlock("[old]\ntype = b2"),
			generated: []string{s3Section},
			want:      managedBlock(s3Section),
		},
		{
			// A file written by the previous version has no markers: its
			// sections match remote names, so they are ours to rewrite. Leaving
			// them would duplicate a section name, which rclone refuses.
			name:      "old-format generated sections are migrated in place",
			existing:  "[s3]\ntype = s3\nregion = old\n",
			generated: []string{s3Section},
			want:      managedBlock(s3Section),
		},
		{
			name:      "no remotes: block removed, operator content untouched",
			existing:  handWritten + "\n\n" + managedBlock(s3Section),
			generated: nil,
			want:      handWritten + "\n",
		},
		{
			name:      "empty file with nothing to generate stays empty",
			existing:  "",
			generated: nil,
			want:      "",
		},
		{
			name:      "a dropped section takes its trailing comments with it",
			existing:  "[s3]\ntype = s3\n# belongs to s3\n[keep]\ntype = ftp\n",
			generated: []string{s3Section},
			want:      "[keep]\ntype = ftp\n\n" + managedBlock(s3Section),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeRcloneConfig(tc.existing, tc.generated)
			if got != tc.want {
				t.Fatalf("mergeRcloneConfig mismatch\ngot:\n%q\nwant:\n%q", got, tc.want)
			}
			// Idempotence: merging the produced file again changes nothing, so
			// a restart cannot accumulate blank lines or duplicate sections.
			if again := mergeRcloneConfig(got, tc.generated); again != got {
				t.Fatalf("merge is not idempotent\ngot:\n%q\nsecond:\n%q", got, again)
			}
		})
	}
}

// A section name backitup does not own must never be dropped, whatever it looks
// like — that is the whole point of the change.
func TestMergeRcloneConfigKeepsUnknownSections(t *testing.T) {
	existing := "[crypt]\ntype = crypt\nremote = provider:\n\n[weird name]\nkey = value\n"
	got := mergeRcloneConfig(existing, []string{"[managed]\ntype = b2\n"})
	for _, want := range []string{"[crypt]", "[weird name]", "remote = provider:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("unknown section content %q was dropped:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "[managed]") {
		t.Fatalf("the managed section is missing:\n%s", got)
	}
}

// The delete guard asks the file, not only the table, what depends on a remote.
func TestSectionsReferencing(t *testing.T) {
	const conf = "# remote = commented-out:\n" +
		"[provider]\n" +
		"type = b2\n" +
		"account = acct\n" +
		"\n" +
		"# >>> backitup managed remotes\n" +
		"[vault]\n" +
		"type = crypt\n" +
		"remote = provider:clients\n" +
		"\n" +
		"[other]\n" +
		"type = crypt\n" +
		"remote = differentscope:\n" +
		"# <<< backitup managed remotes\n" +
		"\n" +
		"[handwritten]\n" +
		"Remote = provider\n"

	cases := []struct {
		name string
		want []string
	}{
		{name: "provider", want: []string{"vault", "handwritten"}},
		{name: "differentscope", want: []string{"other"}},
		{name: "provid", want: nil},        // not a prefix match on a longer word
		{name: "vault", want: nil},         // nothing wraps it
		{name: "commented-out", want: nil}, // a comment is text, not a setting
		{name: "", want: nil},
	}
	for _, tc := range cases {
		got := sectionsReferencing(conf, tc.name)
		if len(got) != len(tc.want) {
			t.Fatalf("sectionsReferencing(%q) = %v, want %v", tc.name, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("sectionsReferencing(%q) = %v, want %v", tc.name, got, tc.want)
			}
		}
	}
}

func TestSectionName(t *testing.T) {
	cases := []struct {
		line string
		name string
		ok   bool
	}{
		{"[s3]", "s3", true},
		{"  [ padded ]  ", "padded", true},
		{"[crypt]", "crypt", true},
		{"name = value", "", false},
		{"[]", "", false},
		{"[a[b]", "", false},
		{"[unterminated", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		name, ok := sectionName(tc.line)
		if name != tc.name || ok != tc.ok {
			t.Errorf("sectionName(%q) = (%q, %v), want (%q, %v)", tc.line, name, ok, tc.name, tc.ok)
		}
	}
}
