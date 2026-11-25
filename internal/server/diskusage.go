package server

import (
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

// diskFigure shapes a stored hot-store measurement for a page: the number, its
// age, and — when the walk stopped at its entry cap — the fact that it is a
// LOWER BOUND rather than a total.
//
// The number is allocated bytes (`diskusage.Measure`: st_blocks of unique
// inodes), so the label next to it says "allocated": it is what the client is
// costing on the volume, never the apparent size of the files, which a sparse
// image or a hardlinked rsync snapshot makes meaningless.
//
// A client that has snapshots but no stored measurement says exactly that. A
// zero would read as "this client costs nothing", which is the defect this
// figure replaced.
func diskFigure(st *model.ClientStorage, snapshots int, now time.Time) (value, note string) {
	if st == nil || st.MeasuredAt.IsZero() {
		if snapshots == 0 {
			return "—", ""
		}
		return "not measured yet", "refreshed by the lifecycle pass"
	}
	age := "as of " + relTime(now.Sub(st.MeasuredAt))
	if st.Partial {
		return "≥ " + humanBytes(st.Bytes), age + " · lower bound: the walk stopped at its entry cap"
	}
	return humanBytes(st.Bytes), age
}
