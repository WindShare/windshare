package eventproject

import (
	"testing"

	v2peer "github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/engine/internal/receive"
	appobs "github.com/windshare/windshare/engine/observation"
)

func TestReceiverTerminationWarningDropsOnlyCallerTeardownResidue(t *testing.T) {
	unknown, closed := v2peer.ReceiverCauseUnknown, v2peer.ReceiverCauseRuntimeClosed
	timeout := v2peer.ReceiverCauseNegotiationTimeout
	caller, none := receive.ReceiverLocalStopCaller, receive.ReceiverLocalStopNone
	for _, test := range []struct {
		name            string
		stop            receive.ReceiverLocalStopReason
		retained        []v2peer.ReceiverCauseClass
		shutdown, drain bool
		want            appobs.FailureCode
	}{
		{name: "caller teardown residue", stop: caller, retained: []v2peer.ReceiverCauseClass{unknown, closed}},
		{name: "unstopped unknown", stop: none, retained: []v2peer.ReceiverCauseClass{unknown}, want: appobs.FailureUnexpected},
		{name: "unstopped runtime closed", stop: none, retained: []v2peer.ReceiverCauseClass{closed}, want: appobs.FailurePeerStopped},
		{name: "completion unknown", stop: receive.ReceiverLocalStopNormalCompletion, retained: []v2peer.ReceiverCauseClass{unknown}, want: appobs.FailureUnexpected},
		{name: "caller with specific class", stop: caller, retained: []v2peer.ReceiverCauseClass{unknown, timeout}, want: appobs.FailurePeerTimeout},
		{name: "caller shutdown failure", stop: caller, retained: []v2peer.ReceiverCauseClass{unknown}, shutdown: true, want: appobs.FailurePeerShutdown},
		{name: "caller drain failure", stop: caller, drain: true, want: appobs.FailurePeerChannelDrain},
		{name: "clean", stop: none},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure, ok := receiverTerminationWarning(test.stop, test.retained, test.shutdown, test.drain)
			if ok != (test.want != 0) || (ok && failure.Code() != test.want) {
				t.Fatalf("warning=%v ok=%v want %v", failure.Code(), ok, test.want)
			}
		})
	}
}
