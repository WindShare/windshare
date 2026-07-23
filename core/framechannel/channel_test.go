package framechannel

import (
	"errors"
	"fmt"
	"testing"
)

func TestSendDispositionDefaultsToAcceptedWhenOwnershipIsUncertain(t *testing.T) {
	transportErr := errors.New("transport failed")
	for name, err := range map[string]error{
		"success":             nil,
		"raw failure":         transportErr,
		"wrapped raw failure": fmt.Errorf("send: %w", transportErr),
	} {
		t.Run(name, func(t *testing.T) {
			if got := SendDispositionOf(err); got != SendAccepted {
				t.Fatalf("disposition=%d, want accepted", got)
			}
		})
	}
}

func TestClassifiedSendFailurePreservesCauseAndDisposition(t *testing.T) {
	cause := errors.New("physical lane unavailable")
	for _, test := range []struct {
		name        string
		classify    func(error) error
		disposition SendDisposition
		marker      error
	}{
		{name: "rejected", classify: RejectSend, disposition: SendRejected, marker: ErrSendRejected},
		{name: "retired", classify: RetireSend, disposition: SendRetired, marker: ErrSendRetired},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := fmt.Errorf("adapter: %w", test.classify(cause))
			if got := SendDispositionOf(err); got != test.disposition {
				t.Fatalf("disposition=%d, want %d", got, test.disposition)
			}
			if !errors.Is(err, cause) || !errors.Is(err, test.marker) {
				t.Fatalf("classified error lost identity: %v", err)
			}
			if got := test.classify(err); got != err {
				t.Fatal("same classification wrapped an already-classified error")
			}
		})
	}
}

func TestClassifiedSendFailureProvidesFallbackCause(t *testing.T) {
	if err := RejectSend(nil); !errors.Is(err, ErrSendRejected) {
		t.Fatalf("nil rejection cause=%v", err)
	}
	if err := RetireSend(nil); !errors.Is(err, ErrSendRetired) {
		t.Fatalf("nil retirement cause=%v", err)
	}
}

func TestClassifiedSendFailureCannotBeReclassified(t *testing.T) {
	cause := errors.New("first authority")
	retired := RetireSend(cause)
	if got := RejectSend(retired); got != retired || SendDispositionOf(got) != SendRetired {
		t.Fatalf("retirement was reclassified: %v disposition=%d", got, SendDispositionOf(got))
	}
	rejected := RejectSend(cause)
	if got := RetireSend(rejected); got != rejected || SendDispositionOf(got) != SendRejected {
		t.Fatalf("rejection was reclassified: %v disposition=%d", got, SendDispositionOf(got))
	}
}
