package model

import "testing"

// RootPath feeds every offsite object path, so a path that repeats the bucket
// writes "<bucket>/<bucket>/<snapshot>". Operators type the bucket name into the
// field labelled "Path inside the bucket", and older builds defaulted that field
// to the client's slug, so the repetition arrives on its own; either way the
// bucket must appear once.
func TestRootPathDoesNotRepeatTheBucket(t *testing.T) {
	cases := []struct {
		bucket, path, want string
	}{
		// The acceptance cases: bucket b, path b|b/sub|sub|"" .
		{"b", "b", "b"},
		{"b", "b/sub", "b/sub"},
		{"b", "sub", "b/sub"},
		{"b", "", "b"},
		// The same repetition with the slashes and spaces an operator types.
		{"b", "/b/", "b"},
		{" b ", " b ", "b"},
		{"b/", "b/sub", "b/sub"},
		// A prefix that merely starts with the bucket's letters is not a repeat.
		{"b", "backups", "b/backups"},
		{"b", "b/backups/b", "b/backups/b"},
		// No bucket (a crypt wrapper, or a hand-written row): unchanged.
		{"", "sub", "sub"},
		{"", "", ""},
		// An absolute bucket — the test-only local backend addresses a directory
		// — keeps its leading slash.
		{"/tmp/bucket", "", "/tmp/bucket"},
		{"/tmp/bucket", "/tmp/bucket", "/tmp/bucket"},
	}
	for _, c := range cases {
		r := Remote{Backend: BackendB2, Config: map[string]string{"bucket": c.bucket, "path": c.path}}
		if got := r.RootPath(); got != c.want {
			t.Errorf("RootPath(bucket=%q, path=%q) = %q, want %q", c.bucket, c.path, got, c.want)
		}
	}
}

// ObjectPath is the one join the upload path, the verification listing and the
// download/delete routes share, so a client directory that repeats the bucket is
// dropped in all of them or in none.
func TestObjectPathDropsADirectoryThatRepeatsTheBucket(t *testing.T) {
	cases := []struct{ root, dir, name, want string }{
		{"b", "b", "20260101T000000Z.tar.gz", "b/20260101T000000Z.tar.gz"},
		{"b/backups", "b", "x.tar.gz", "b/backups/x.tar.gz"},
		{"b", "client-a", "x.tar.gz", "b/client-a/x.tar.gz"},
		{"b", "", "x.tar.gz", "b/x.tar.gz"},
		{"", "client-a", "x.tar.gz", "client-a/x.tar.gz"},
		{"", "", "x.tar.gz", "x.tar.gz"},
		{"/tmp/dir", "", "x.tar.gz", "/tmp/dir/x.tar.gz"},
	}
	for _, c := range cases {
		if got := ObjectPath(c.root, c.dir, c.name); got != c.want {
			t.Errorf("ObjectPath(%q, %q, %q) = %q, want %q", c.root, c.dir, c.name, got, c.want)
		}
	}
	if got := FirstSegment("b/backups/c"); got != "b" {
		t.Errorf("FirstSegment(\"b/backups/c\") = %q, want %q", got, "b")
	}
}
