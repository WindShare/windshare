package framechannel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
)

type capacityEvidence struct{ remaining time.Duration }

func (evidence *capacityEvidence) CapacityWait() time.Duration { return evidence.remaining }

func TestCapacityEvidencePreservesFailureIdentityAndExpires(t *testing.T) {
	evidence := &capacityEvidence{remaining: 15 * time.Second}
	deadline := fmt.Errorf("content progress: %w", context.DeadlineExceeded)
	classified := ClassifyDeadline(evidence, deadline)
	wait, ok := errors.AsType[*BandwidthWaitError](classified)
	if !ok || wait.RetryAfter != evidence.remaining || !errors.Is(classified, context.DeadlineExceeded) {
		t.Fatalf("capacity evidence lost deadline or recovery delay: %v", classified)
	}
	if wait.Error() != "relay bandwidth is busy: "+deadline.Error() {
		t.Fatal("diagnostic lost the original deadline context")
	}
	if WithCapacityWait(evidence, classified) != classified {
		t.Fatal("nested recovery replaced the original capacity decision")
	}
	evidence.remaining = 0
	if got := ClassifyDeadline(evidence, deadline); got != deadline {
		t.Fatalf("expired congestion evidence masked a real failure: %v", got)
	}
}

func TestCapacityEvidenceNeverReclassifiesCancellationOrTransportFailure(t *testing.T) {
	evidence := &capacityEvidence{remaining: time.Minute}
	for _, failure := range []error{nil, context.Canceled, io.EOF, io.ErrClosedPipe, errors.New("authentication refused")} {
		if got := ClassifyDeadline(evidence, failure); got != failure {
			t.Fatalf("capacity evidence changed %v to %v", failure, got)
		}
	}
	if WithCapacityWait(evidence, nil) != nil {
		t.Fatal("capacity evidence manufactured a failure")
	}
	for _, channel := range []any{nil, struct{}{}, &capacityEvidence{remaining: -time.Second}} {
		if got := ClassifyDeadline(channel, context.DeadlineExceeded); got != context.DeadlineExceeded {
			t.Fatalf("channel without live evidence masked a deadline: %v", got)
		}
	}
}
