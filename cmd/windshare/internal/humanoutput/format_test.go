package humanoutput

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
)

func TestFormatBytesUsesDecimalUnitsWithoutFloatLoss(t *testing.T) {
	t.Parallel()
	tests := []struct {
		value uint64
		want  string
	}{
		{0, "0 B"}, {999, "999 B"}, {1_000, "1.0 KB"}, {1_249, "1.2 KB"},
		{1_250, "1.3 KB"}, {8_200_000, "8.2 MB"}, {142_500_000, "142.5 MB"},
		{1_999_999_999, "2.0 GB"}, {math.MaxUint64, "18446744073.7 GB"},
	}
	for _, test := range tests {
		if got := FormatBytes(test.value); got != test.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", test.value, got, test.want)
		}
	}
}

func TestDurationFormattersAreDeterministic(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		got  string
		want string
	}{
		{"negative elapsed", FormatElapsed(-time.Second), "0.0s"},
		{"fractional elapsed", FormatElapsed(9*time.Second + 550*time.Millisecond), "9.6s"},
		{"minute elapsed", FormatElapsed(65 * time.Second), "1m 5s"},
		{"short eta", FormatETA(2), "2s left"},
		{"minute eta", FormatETA(125), "2m 5s left"},
		{"hour eta", FormatETA(3_725), "1h 2m left"},
	} {
		if test.got != test.want {
			t.Errorf("%s = %q, want %q", test.name, test.got, test.want)
		}
	}
}

func TestFailureMessagesCoverClosedVocabulary(t *testing.T) {
	t.Parallel()
	codes := []clievent.FailureCode{
		clievent.FailureUnexpected, clievent.FailureCanceled, clievent.FailureDeadline,
		clievent.FailureInvalidInput, clievent.FailureCapabilityInvalid, clievent.FailureSelectionMissing,
		clievent.FailurePublication, clievent.FailureTraceWrite, clievent.FailureRelayMalformed,
		clievent.FailureRelayStarting, clievent.FailurePeerNegotiation, clievent.FailureSourceUnavailable,
		clievent.FailureSourceRevisionChanged, clievent.FailureCatalogUnavailable,
		clievent.FailureSessionProtocol, clievent.FailureOutputStateIO, clievent.FailureCheckpointBusy,
	}
	for _, code := range codes {
		message := failureMessage(mustFailure(t, code))
		if message == "" || strings.Contains(message, "unexpected") && code != clievent.FailureUnexpected {
			t.Errorf("failureMessage(%v) = %q", code, message)
		}
	}
	retryable, err := clievent.NewRetryableFailure(clievent.FailureRelayStarting, 1_001)
	if err != nil {
		t.Fatal(err)
	}
	if got := failureMessage(retryable); !strings.Contains(got, "Retry after 2s") {
		t.Fatalf("retryable message = %q", got)
	}
}
