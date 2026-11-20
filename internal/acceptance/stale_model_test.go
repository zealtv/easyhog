//go:build unix

package acceptance

import (
	"strings"
	"testing"
	"time"
)

// TestPaletteActsOnWhatTheRefresherHasRead pins the harness rule behind the
// reopening step's Linux CI failure (2026-11-19): when the refresher has read an
// instance and the model has not yet been fed that read, the row still says
// "not inspected" and the palette offers no Run. A reader's palette always sees
// the current view, because RunLive feeds it; w.palette does the same.
func TestPaletteActsOnWhatTheRefresherHasRead(t *testing.T) {
	w := newWorld(t, gate(t))
	f := w.build(t)
	w.start()
	w.selectRow("alpha")
	// Read, and deliberately not synced into the model.
	w.await("the refresher's first read of alpha", 20*time.Second, func() bool { return w.view("alpha").Inspection.Last != nil })
	if !strings.Contains(w.screen(), "not inspected") {
		t.Fatalf("setup: the model already shows the read, so this does not exercise the gap\n%s", w.screen())
	}
	w.palette("run")
	if !w.said("Run: alpha: ticked") || !w.ticking(f.alpha) {
		t.Errorf("Run never reached the ticker\n%s", w.screen())
	}
}
