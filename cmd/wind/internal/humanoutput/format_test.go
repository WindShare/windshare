package humanoutput

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/engine"
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
		{999_949, "999.9 KB"}, {999_950, "1.0 MB"}, {999_999, "1.0 MB"},
		{999_949_999, "999.9 MB"}, {999_950_000, "1.0 GB"}, {999_999_999, "1.0 GB"},
		{999_999_999_999, "1000.0 GB"},
	}
	for _, test := range tests {
		if got := FormatBytes(test.value); got != test.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", test.value, got, test.want)
		}
	}
}

func TestFormatRateRollsOverLikeBytes(t *testing.T) {
	t.Parallel()
	if got := FormatRate(999_960); got != "1.0 MB/s" {
		t.Errorf("FormatRate(999960) = %q, want %q", got, "1.0 MB/s")
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
		{"last tenth before a minute", FormatElapsed(59*time.Second + 949*time.Millisecond), "59.9s"},
		{"rounds up to a minute", FormatElapsed(59*time.Second + 950*time.Millisecond), "1m 0s"},
		{"exactly a minute", FormatElapsed(time.Minute), "1m 0s"},
		{"rounds half a second up", FormatElapsed(65*time.Second + 500*time.Millisecond), "1m 6s"},
		{"maximum elapsed does not wrap", FormatElapsed(math.MaxInt64), "2562047h 47m"},
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
	codes := []engine.FailureCode{
		engine.FailureUnexpected, engine.FailureCanceled, engine.FailureDeadline,
		engine.FailureInvalidInput, engine.FailureNetworkUnavailable, engine.FailureCapabilityInvalid,
		engine.FailureSelectionMissing, engine.FailureSelectionCollision, engine.FailurePublication,
		engine.FailureAccountStorageError, engine.FailureRelayRejected, engine.FailureRelayUnavailable,
		engine.FailureRelayNotFound, engine.FailureRelayStopped, engine.FailureRelayAccessDenied,
		engine.FailureRelayRouteNotFound, engine.FailureShareNotReady, engine.FailureShareStopped, engine.FailureDirectUnavailable,
		engine.FailureDirectPolicy, engine.FailureDirectBusy, engine.FailureDirectAdmission,
		engine.FailureSourceUnavailable, engine.FailureSourceChanged, engine.FailureSourceMissing,
		engine.FailureSourceAccessDenied, engine.FailureSourceUnsupported, engine.FailureSourceOmitted,
		engine.FailureCatalogUnavailable, engine.FailureSessionFailed, engine.FailureOutputFailed,
		engine.FailureOutputBusy, engine.FailureOutputOperationAlreadyRunning,
		engine.FailureOutputNeedsAttention, engine.FailureRetainedOutputMissing, engine.FailureDestinationCollision,
		engine.FailureCheckpointFailed, engine.FailureCheckpointRevisionConflict, engine.FailureCheckpointInvalid,
		engine.FailureOwnedObjectUnknown,
	}
	for _, code := range codes {
		message := failureMessage(engine.Failure{Code: code})
		if message == "" || strings.Contains(message, "unexpected") && code != engine.FailureUnexpected {
			t.Errorf("failureMessage(%v) = %q", code, message)
		}
	}
	retryable := engine.Failure{Code: engine.FailureRelayUnavailable, RetryAfter: 1001 * time.Millisecond}
	if got := failureMessage(retryable); !strings.Contains(got, "Retry after 2s") {
		t.Fatalf("retryable message = %q", got)
	}
	notReady := engine.Failure{Code: engine.FailureShareNotReady, RetryAfter: 250 * time.Millisecond}
	if got := failureMessage(notReady); got != "The share is not ready yet. Retry after 1s." {
		t.Fatalf("share not ready message = %q", got)
	}
}
