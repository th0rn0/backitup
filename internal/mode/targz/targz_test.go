package targz

import "testing"

func TestClientModeName(t *testing.T) {
	if (Mode{}).Mode() != "targz" {
		t.Fatalf("mode = %q, want targz", (Mode{}).Mode())
	}
}
