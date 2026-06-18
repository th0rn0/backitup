package targz

import "testing"

func TestExcluded(t *testing.T) {
	patterns := []string{"*.tmp", "node_modules", "cache/*"}
	cases := []struct {
		rel  string
		want bool
	}{
		{"keep.txt", false},
		{"scratch.tmp", true},      // *.tmp by base
		{"node_modules", true},     // dir by base
		{"sub/node_modules", true}, // dir by base, nested
		{"cache/x", true},          // cache/* by rel
		{"src/main.go", false},     // not excluded
		{"a/b/deep.tmp", true},     // *.tmp on base of nested
	}
	for _, c := range cases {
		if got := excluded(c.rel, patterns); got != c.want {
			t.Errorf("excluded(%q) = %v, want %v", c.rel, got, c.want)
		}
	}
}

func TestModeName(t *testing.T) {
	if (Mode{}).Mode() != "targz" {
		t.Fatalf("mode = %q, want targz", (Mode{}).Mode())
	}
}

func TestTrimSlash(t *testing.T) {
	for in, want := range map[string]string{"a/": "a", "b//": "b", "c": "c", "/": "/"} {
		if got := trimSlash(in); got != want {
			t.Errorf("trimSlash(%q) = %q, want %q", in, got, want)
		}
	}
}
