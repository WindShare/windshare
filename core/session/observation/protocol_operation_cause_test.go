package sessionobs

import "testing"

const unknownProtocolOperationCause ProtocolOperationCause = 255

func TestProtocolOperationCauseIsLifecycle(t *testing.T) {
	for _, test := range []struct {
		cause ProtocolOperationCause
		want  bool
	}{
		{ProtocolOperationCauseNone, true},
		{ProtocolOperationCauseCanceled, true},
		{ProtocolOperationCauseDeadline, false},
		{ProtocolOperationCauseRuntimeClosed, false},
		{ProtocolOperationCauseLaneUnavailable, false},
		{ProtocolOperationCauseWriterStopped, false},
		{ProtocolOperationCauseOperationClosed, true},
		{ProtocolOperationCauseProtocolFailure, false},
		{ProtocolOperationCauseSuperseded, true},
		{unknownProtocolOperationCause, false},
	} {
		t.Run(test.cause.DiagnosticName(), func(t *testing.T) {
			if got := test.cause.IsLifecycle(); got != test.want {
				t.Fatalf("IsLifecycle() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestProtocolOperationCauseMerge(t *testing.T) {
	for _, test := range []struct {
		name    string
		current ProtocolOperationCause
		next    ProtocolOperationCause
		want    ProtocolOperationCause
	}{
		{"empty evidence", ProtocolOperationCauseNone, ProtocolOperationCauseNone, ProtocolOperationCauseNone},
		{"initial lifecycle", ProtocolOperationCauseNone, ProtocolOperationCauseSuperseded, ProtocolOperationCauseSuperseded},
		{"initial failure", ProtocolOperationCauseNone, ProtocolOperationCauseDeadline, ProtocolOperationCauseDeadline},
		{"empty preserves lifecycle", ProtocolOperationCauseCanceled, ProtocolOperationCauseNone, ProtocolOperationCauseCanceled},
		{"empty preserves failure", ProtocolOperationCauseDeadline, ProtocolOperationCauseNone, ProtocolOperationCauseDeadline},
		{"first lifecycle wins", ProtocolOperationCauseCanceled, ProtocolOperationCauseOperationClosed, ProtocolOperationCauseCanceled},
		{"lifecycle order matters", ProtocolOperationCauseOperationClosed, ProtocolOperationCauseCanceled, ProtocolOperationCauseOperationClosed},
		{"fault replaces cancellation", ProtocolOperationCauseCanceled, ProtocolOperationCauseProtocolFailure, ProtocolOperationCauseProtocolFailure},
		{"runtime failure replaces closure", ProtocolOperationCauseOperationClosed, ProtocolOperationCauseRuntimeClosed, ProtocolOperationCauseRuntimeClosed},
		{"writer failure replaces supersession", ProtocolOperationCauseSuperseded, ProtocolOperationCauseWriterStopped, ProtocolOperationCauseWriterStopped},
		{"cancellation preserves failure", ProtocolOperationCauseProtocolFailure, ProtocolOperationCauseCanceled, ProtocolOperationCauseProtocolFailure},
		{"closure preserves failure", ProtocolOperationCauseRuntimeClosed, ProtocolOperationCauseOperationClosed, ProtocolOperationCauseRuntimeClosed},
		{"first failure wins", ProtocolOperationCauseDeadline, ProtocolOperationCauseProtocolFailure, ProtocolOperationCauseDeadline},
		{"failure order matters", ProtocolOperationCauseProtocolFailure, ProtocolOperationCauseDeadline, ProtocolOperationCauseProtocolFailure},
		{"repeated evidence", ProtocolOperationCauseLaneUnavailable, ProtocolOperationCauseLaneUnavailable, ProtocolOperationCauseLaneUnavailable},
		{"unknown is not lifecycle evidence", ProtocolOperationCauseCanceled, unknownProtocolOperationCause, unknownProtocolOperationCause},
		{"cancellation preserves unknown", unknownProtocolOperationCause, ProtocolOperationCauseCanceled, unknownProtocolOperationCause},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.current.Merge(test.next); got != test.want {
				t.Fatalf("Merge(%s, %s) = %s, want %s",
					test.current.DiagnosticName(), test.next.DiagnosticName(), got.DiagnosticName(), test.want.DiagnosticName())
			}
		})
	}
}
