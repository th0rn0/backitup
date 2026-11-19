package targz

import (
	"errors"
	"strings"
	"testing"
)

// An upload the server refuses (over the byte cap, or with no free space) makes
// our next write fail first, so the local error is only ever a broken pipe. The
// operator needs the server's reason — it names the limit — in the run's log
// tail, which is what this composition is for.
func TestUploadErrorCarriesTheRemoteReason(t *testing.T) {
	brokenPipe := errors.New("write |1: broken pipe")
	exitErr := errors.New("exit status 1")

	const capMsg = "backitup-recv: upload exceeds BACKITUP_RECV_MAX_BYTES (1048576 bytes); " +
		"aborted after 1048577 bytes, partial archive removed, no snapshot written"

	cases := []struct {
		name       string
		archiveErr error
		waitErr    error
		remote     string
		wantNil    bool
		want       []string
		wantWrap   error
	}{
		{
			name:       "the stream died because the server aborted it",
			archiveErr: brokenPipe,
			waitErr:    exitErr,
			remote:     capMsg + "\n",
			want:       []string{"broken pipe", "server said", "BACKITUP_RECV_MAX_BYTES (1048576 bytes)"},
			wantWrap:   brokenPipe,
		},
		{
			name:       "a local failure with nothing to add from the server",
			archiveErr: brokenPipe,
			want:       []string{"broken pipe"},
			wantWrap:   brokenPipe,
		},
		{
			name:    "the server failed without our writing anything",
			waitErr: exitErr,
			remote:  "backitup-recv: refusing to start: 1024 bytes free on the backup volume, below BACKITUP_RECV_MIN_FREE_BYTES (2147483648); nothing written\n",
			want:    []string{"remote upload failed", "BACKITUP_RECV_MIN_FREE_BYTES (2147483648)"},
			// The exit status must stay reachable so callers can inspect it.
			wantWrap: exitErr,
		},
		{
			name:    "a clean run",
			wantNil: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := uploadError(tc.archiveErr, tc.waitErr, tc.remote)
			if tc.wantNil {
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not mention %q", err, want)
				}
			}
			if !errors.Is(err, tc.wantWrap) {
				t.Fatalf("error %q does not wrap %v", err, tc.wantWrap)
			}
		})
	}
}
