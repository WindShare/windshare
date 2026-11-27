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
		for _, pair := range []struct {
			name string
			got  string
		}{
			{"failureMessage", failureMessage(engine.Failure{Code: code})},
			{"failureReason", failureReason(engine.Failure{Code: code})},
		} {
			if pair.got == "" || strings.Contains(pair.got, "unexpected") && code != engine.FailureUnexpected {
				t.Errorf("%s(%v) = %q", pair.name, code, pair.got)
			}
		}
	}
	for _, tc := range []struct {
		name    string
		failure engine.Failure
		want    string
	}{
		{
			name:    "sub-second share not ready polling hint is omitted",
			failure: engine.Failure{Code: engine.FailureShareNotReady, RetryAfter: 250 * time.Millisecond},
			want:    "The share is not ready yet.",
		},
		{
			name:    "sub-second relay admission polling hint is omitted",
			failure: engine.Failure{Code: engine.FailureRelayUnavailable, RetryAfter: 250 * time.Millisecond},
			want:    "The relay is unavailable.",
		},
		{
			name:    "999ms stays below human cooldown threshold",
			failure: engine.Failure{Code: engine.FailureRelayUnavailable, RetryAfter: 999 * time.Millisecond},
			want:    "The relay is unavailable.",
		},
		{
			name:    "exact one-second cooldown renders",
			failure: engine.Failure{Code: engine.FailureRelayUnavailable, RetryAfter: time.Second},
			want:    "The relay is unavailable. Retry after 1s.",
		},
		{
			name:    "fractional multi-second cooldown rounds up",
			failure: engine.Failure{Code: engine.FailureRelayUnavailable, RetryAfter: 1001 * time.Millisecond},
			want:    "The relay is unavailable. Retry after 2s.",
		},
		{
			name:    "multi-second share not ready cooldown renders",
			failure: engine.Failure{Code: engine.FailureShareNotReady, RetryAfter: 5 * time.Second},
			want:    "The share is not ready yet. Retry after 5s.",
		},
		{
			name:    "relay not found keeps terminal recovery advice",
			failure: engine.Failure{Code: engine.FailureRelayNotFound},
			want:    "The share is not currently available. Retry the original link.",
		},
	} {
		if got := failureMessage(tc.failure); got != tc.want {
			t.Errorf("%s: failureMessage = %q, want %q", tc.name, got, tc.want)
		}
	}
	for _, tc := range []struct {
		name    string
		failure engine.Failure
		want    string
	}{
		{
			name:    "relay not found reason omits manual retry advice",
			failure: engine.Failure{Code: engine.FailureRelayNotFound},
			want:    "The share is not currently available.",
		},
		{
			name:    "relay unavailable reason omits retry after",
			failure: engine.Failure{Code: engine.FailureRelayUnavailable, RetryAfter: 30 * time.Second},
			want:    "The relay is unavailable.",
		},
		{
			name:    "network unavailable reason omits manual retry advice",
			failure: engine.Failure{Code: engine.FailureNetworkUnavailable},
			want:    "The WindShare network is unavailable.",
		},
		{
			name:    "source path survives reason formatting",
			failure: engine.Failure{Code: engine.FailureSourceUnavailable, SourcePath: "notes.txt"},
			want:    "The source content is unavailable. Path: notes.txt",
		},
	} {
		if got := failureReason(tc.failure); got != tc.want {
			t.Errorf("%s: failureReason = %q, want %q", tc.name, got, tc.want)
		}
	}
}
