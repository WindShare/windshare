package relaysetup

import (
	"io"
	"math"
	"testing"
	"time"
)

type interruptedBody struct{}

func (interruptedBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestRetryDelayBoundsBeforeDurationConversion(t *testing.T) {
	for _, test := range []struct {
		milliseconds int64
		expected     time.Duration
	}{
		{math.MaxInt64, maximumRetry},
		{math.MinInt64, time.Second},
		{5000, 5 * time.Second},
	} {
		if got := (&apiFailure{RetryMS: test.milliseconds}).delay(); got != test.expected {
			t.Fatalf("retry %d = %s; expected %s", test.milliseconds, got, test.expected)
		}
	}
}
