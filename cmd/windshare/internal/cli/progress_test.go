package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestProgressReportsSelectedBytesAcrossResume(t *testing.T) {
	var output bytes.Buffer
	progress := newProgress(&output, 1000, 400)
	progress.step(100)
	received, _ := progress.done()
	if received != 100 {
		t.Fatalf("received bytes = %d, want 100", received)
	}
	if got := output.String(); !strings.Contains(got, "500 B/1000 B") {
		t.Fatalf("progress output = %q, want resumed selected-byte progress", got)
	}
}

func TestProgressClampsDisplayToSelection(t *testing.T) {
	var output bytes.Buffer
	progress := newProgress(&output, 10, 0)
	progress.step(20)
	if got := output.String(); !strings.Contains(got, "10 B/10 B") {
		t.Fatalf("progress output = %q, want selection-clamped display", got)
	}
}

func TestCompletedSelectedBytesExcludesBoundarySiblings(t *testing.T) {
	plan, _ := newJournalPlan(t, []string{"a.txt"})
	plan.Sink().Have().Set(0) // a.txt and b.txt share this packed-stream chunk.
	completed, err := completedSelectedBytes(plan)
	if err != nil {
		t.Fatal(err)
	}
	if completed != 4 || completed != plan.SelectedBytes() {
		t.Fatalf("completed selected bytes = %d, plan total = %d", completed, plan.SelectedBytes())
	}
}
