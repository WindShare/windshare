package harness

import "testing"

func TestEventPrecedes(t *testing.T) {
	t.Parallel()

	events := []string{"channel-open", "terminal-sent", "terminal-acknowledged", "channel-closed"}
	tests := []struct {
		name   string
		before string
		after  string
		want   bool
	}{
		{name: "ordered", before: "terminal-sent", after: "channel-closed", want: true},
		{name: "reversed", before: "channel-closed", after: "terminal-sent", want: false},
		{name: "missing before", before: "missing", after: "channel-closed", want: false},
		{name: "missing after", before: "terminal-sent", after: "missing", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := eventPrecedes(events, test.before, test.after); got != test.want {
				t.Fatalf("eventPrecedes(%q, %q) = %t, want %t", test.before, test.after, got, test.want)
			}
		})
	}
}

func TestObservationSnapshotDoesNotAliasRecorder(t *testing.T) {
	t.Parallel()

	recorder := newObservationRecorder()
	recorder.event("first")
	recorder.fail("failure")
	snapshot := recorder.snapshot()
	snapshot.Events[0] = "mutated"
	snapshot.Errors[0] = "mutated"

	next := recorder.snapshot()
	if next.Events[0] != "first" || next.Errors[0] != "failure" {
		t.Fatalf("snapshot mutation leaked into recorder: %+v", next)
	}
}
