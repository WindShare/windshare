package webrtc

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/framechannel"
)

func TestWebRTCLifecycleSourceShapes(t *testing.T) {
	acceptedTerminal := &sendAdmission{
		id: 1, operation: sendTerminal, state: sendAdmissionAccepted,
	}
	terminalAccepted, ok := sendResolutionLifecycleTrace(
		21, acceptedTerminal, framechannel.Open, LifecycleTerminalLocalPending,
	)
	if !ok {
		t.Fatal("terminal acceptance was suppressed")
	}
	rejected := &sendAdmission{
		id: 2, operation: sendOrdinary, state: sendAdmissionRefused,
		err: framechannel.RejectSend(context.Canceled),
	}
	rejectedTrace, ok := sendResolutionLifecycleTrace(
		21, rejected, framechannel.Open, LifecycleTerminalNone,
	)
	if !ok {
		t.Fatal("send rejection was suppressed")
	}
	retired := &sendAdmission{
		id: 3, operation: sendOrdinary, state: sendAdmissionRefused,
		err: framechannel.RetireSend(ErrChannelClosed),
	}
	retiredTrace, ok := sendResolutionLifecycleTrace(
		21, retired, framechannel.Open, LifecycleTerminalLocalPending,
	)
	if !ok {
		t.Fatal("send retirement was suppressed")
	}
	acceptedFailure := acceptedTransportFailureLifecycleTrace(
		21,
		&sendAdmission{id: 4, operation: sendOrdinary, state: sendAdmissionAccepted},
		framechannel.Open,
		LifecycleTerminalNone,
	)
	connectingTerminalRefusal, _ := sendResolutionLifecycleTrace(
		21,
		&sendAdmission{id: 5, operation: sendTerminal, state: sendAdmissionRefused, err: framechannel.RejectSend(ErrChannelNotOpen)},
		framechannel.Connecting,
		LifecycleTerminalNone,
	)
	closedTerminalRetirement, _ := sendResolutionLifecycleTrace(
		21,
		&sendAdmission{id: 6, operation: sendTerminal, state: sendAdmissionRefused, err: framechannel.RetireSend(ErrChannelClosed)},
		framechannel.Closed,
		LifecycleTerminalLocalPending,
	)

	last := channelLifecycleTrace(
		21,
		LifecycleTransitionClosedFailed,
		framechannel.Closed,
		LifecycleTerminalNone,
		ErrTransport,
	)
	tests := []struct {
		name string
		got  LifecycleTrace
		want LifecycleTrace
	}{
		{
			"send accepted",
			terminalAccepted,
			LifecycleTrace{
				ChannelID: 21, OperationID: 1,
				Operation: LifecycleOperationSendTerminal, Transition: LifecycleTransitionSendAccepted,
				Disposition: framechannel.SendAccepted, State: framechannel.Open,
				Terminal: LifecycleTerminalLocalPending, Cause: LifecycleCauseNone,
			},
		},
		{
			"accepted provider failure",
			acceptedFailure,
			LifecycleTrace{
				ChannelID: 21, OperationID: 4,
				Operation: LifecycleOperationSend, Transition: LifecycleTransitionSendAccepted,
				Disposition: framechannel.SendAccepted, State: framechannel.Open,
				Terminal: LifecycleTerminalNone, Cause: LifecycleCauseTransport,
			},
		},
		{
			"send rejected",
			rejectedTrace,
			LifecycleTrace{
				ChannelID: 21, OperationID: 2,
				Operation: LifecycleOperationSend, Transition: LifecycleTransitionSendRejected,
				Disposition: framechannel.SendRejected, State: framechannel.Open,
				Terminal: LifecycleTerminalNone, Cause: LifecycleCauseCanceled,
			},
		},
		{
			"send retired",
			retiredTrace,
			LifecycleTrace{
				ChannelID: 21, OperationID: 3,
				Operation: LifecycleOperationSend, Transition: LifecycleTransitionSendRetired,
				Disposition: framechannel.SendRetired, State: framechannel.Open,
				Terminal: LifecycleTerminalLocalPending, Cause: LifecycleCauseNaturalRetirement,
			},
		},
		{
			"connecting terminal refusal",
			connectingTerminalRefusal,
			LifecycleTrace{
				ChannelID: 21, OperationID: 5,
				Operation: LifecycleOperationSendTerminal, Transition: LifecycleTransitionSendRejected,
				Disposition: framechannel.SendRejected, State: framechannel.Connecting,
				Terminal: LifecycleTerminalNone, Cause: LifecycleCauseNotOpen,
			},
		},
		{
			"closed terminal retirement",
			closedTerminalRetirement,
			LifecycleTrace{
				ChannelID: 21, OperationID: 6,
				Operation: LifecycleOperationSendTerminal, Transition: LifecycleTransitionSendRetired,
				Disposition: framechannel.SendRetired, State: framechannel.Closed,
				Terminal: LifecycleTerminalLocalPending, Cause: LifecycleCauseNaturalRetirement,
			},
		},
		{
			"remote terminal reserved",
			channelLifecycleTrace(21, LifecycleTransitionRemoteTerminalReserved, framechannel.Open, LifecycleTerminalNone, nil),
			LifecycleTrace{
				ChannelID: 21, Operation: LifecycleOperationChannel,
				Transition: LifecycleTransitionRemoteTerminalReserved, State: framechannel.Open,
				Terminal: LifecycleTerminalRemotePending, Cause: LifecycleCauseNone,
			},
		},
		{
			"termination pending",
			channelLifecycleTrace(21, LifecycleTransitionTerminationPending, framechannel.Open, LifecycleTerminalNone, ErrRemoteClosed),
			LifecycleTrace{
				ChannelID: 21, Operation: LifecycleOperationChannel,
				Transition: LifecycleTransitionTerminationPending, State: framechannel.Open,
				Terminal: LifecycleTerminalNone, Cause: LifecycleCauseRemoteClosed,
			},
		},
		{
			"connecting termination pending",
			channelLifecycleTrace(21, LifecycleTransitionTerminationPending, framechannel.Connecting, LifecycleTerminalNone, ErrRemoteClosed),
			LifecycleTrace{
				ChannelID: 21, Operation: LifecycleOperationChannel,
				Transition: LifecycleTransitionTerminationPending, State: framechannel.Connecting,
				Terminal: LifecycleTerminalNone, Cause: LifecycleCauseRemoteClosed,
			},
		},
		{
			"closed clean",
			channelLifecycleTrace(21, LifecycleTransitionClosedClean, framechannel.Closed, LifecycleTerminalNone, nil),
			LifecycleTrace{
				ChannelID: 21, Operation: LifecycleOperationChannel,
				Transition: LifecycleTransitionClosedClean, State: framechannel.Closed,
				Terminal: LifecycleTerminalNone, Cause: LifecycleCauseNone,
			},
		},
		{
			"closed failed",
			last,
			LifecycleTrace{
				ChannelID: 21, Operation: LifecycleOperationChannel,
				Transition: LifecycleTransitionClosedFailed, State: framechannel.Closed,
				Terminal: LifecycleTerminalNone, Cause: LifecycleCauseTransport,
			},
		},
		{
			"trace dropped",
			lifecycleTraceDropNotice(last, 5),
			LifecycleTrace{
				ChannelID: 21, Operation: LifecycleOperationChannel,
				Transition: LifecycleTransitionTraceDropped, State: framechannel.Closed,
				Terminal: LifecycleTerminalNone, Cause: LifecycleCauseNone, Dropped: 5,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !reflect.DeepEqual(test.got, test.want) {
				t.Fatalf("source trace = %+v, want %+v", test.got, test.want)
			}
			if violation := ValidateLifecycleTrace(test.got); violation != LifecycleContractValid {
				t.Fatalf("source contract violation=%d trace=%+v", violation, test.got)
			}
		})
	}

	ordinarySuccess := &sendAdmission{
		id: 5, operation: sendOrdinary, state: sendAdmissionAccepted,
	}
	if trace, emitted := sendResolutionLifecycleTrace(
		21, ordinarySuccess, framechannel.Open, LifecycleTerminalNone,
	); emitted || trace != (LifecycleTrace{}) {
		t.Fatalf("ordinary success source trace = %+v emitted=%t", trace, emitted)
	}
}

func TestWebRTCLifecycleNilTracerHasNoDispatcher(t *testing.T) {
	fake := newFakeDataChannel(pion.DataChannelStateOpen)
	channel, err := newChannelWithRuntime(fake, defaultFlowControl, channelRuntime{})
	if err != nil {
		t.Fatalf("construct channel: %v", err)
	}
	if channel.traces != nil || channel.lifecycle.traces != nil {
		t.Fatal("nil tracer allocated a WebRTC lifecycle dispatcher")
	}
	if err := channel.Close(); err != nil {
		t.Fatalf("close channel: %v", err)
	}
}

func TestWebRTCLifecycleDropCounterSaturates(t *testing.T) {
	dispatcher := &lifecycleTraceDispatcher{
		tracer: LifecycleTraceFunc(func(LifecycleTrace) {}),
		queue:  make([]LifecycleTrace, lifecycleTraceQueueCapacity),
		loss:   LifecycleObservationLoss{QueueOverflow: ^uint64(0)},
	}
	dispatcher.wake = sync.NewCond(&dispatcher.mu)
	dispatcher.emit(LifecycleTrace{ChannelID: 1})
	if dispatcher.loss.QueueOverflow != ^uint64(0) {
		t.Fatalf("drop count wrapped to %d", dispatcher.loss.QueueOverflow)
	}
}

func TestWebRTCLifecycleValidatorReturnsClosedMutationReasons(t *testing.T) {
	base := channelLifecycleTrace(
		21, LifecycleTransitionTerminationPending,
		framechannel.Connecting, LifecycleTerminalNone, ErrRemoteClosed,
	)
	tests := []struct {
		name string
		edit func(*LifecycleTrace)
		want LifecycleContractViolation
	}{
		{"unknown transition", func(value *LifecycleTrace) { value.Transition = "future" }, LifecycleContractUnknownEnum},
		{"missing channel", func(value *LifecycleTrace) { value.ChannelID = 0 }, LifecycleContractInvalidIdentity},
		{"closed pending state", func(value *LifecycleTrace) { value.State = framechannel.Closed }, LifecycleContractInvalidStageFields},
		{"pending without cause", func(value *LifecycleTrace) { value.Cause = LifecycleCauseNone }, LifecycleContractInvalidStageFields},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := base
			test.edit(&mutated)
			if got := ValidateLifecycleTrace(mutated); got != test.want {
				t.Fatalf("violation=%d want=%d trace=%+v", got, test.want, mutated)
			}
		})
	}
}

func TestWebRTCLifecycleValidatorCoversProducerRefusalMatrix(t *testing.T) {
	tests := []struct {
		name      string
		operation sendOperation
		state     framechannel.ChannelState
		terminal  LifecycleTerminalState
		err       error
	}{
		{"ordinary connecting rejection", sendOrdinary, framechannel.Connecting, LifecycleTerminalNone, framechannel.RejectSend(ErrChannelNotOpen)},
		{"terminal connecting rejection", sendTerminal, framechannel.Connecting, LifecycleTerminalNone, framechannel.RejectSend(ErrChannelNotOpen)},
		{"ordinary closed retirement", sendOrdinary, framechannel.Closed, LifecycleTerminalNone, framechannel.RetireSend(ErrChannelClosed)},
		{"terminal closed rejection", sendTerminal, framechannel.Closed, LifecycleTerminalLocalPending, framechannel.RejectSend(ErrTransport)},
		{"ordinary local-terminal retirement", sendOrdinary, framechannel.Open, LifecycleTerminalLocalPending, framechannel.RetireSend(ErrChannelClosed)},
		{"terminal remote-terminal retirement", sendTerminal, framechannel.Open, LifecycleTerminalRemotePending, framechannel.RetireSend(ErrChannelClosed)},
		{"terminal open rejection", sendTerminal, framechannel.Open, LifecycleTerminalNone, framechannel.RejectSend(context.Canceled)},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			trace, emitted := sendResolutionLifecycleTrace(
				21,
				&sendAdmission{id: uint64(index + 1), operation: test.operation, state: sendAdmissionRefused, err: test.err},
				test.state,
				test.terminal,
			)
			if !emitted {
				t.Fatal("producer refusal was suppressed")
			}
			if violation := ValidateLifecycleTrace(trace); violation != LifecycleContractValid {
				t.Fatalf("source contract violation=%d trace=%+v", violation, trace)
			}
		})
	}
}

func TestWebRTCLifecycleCauseOrderingKeepsRetirementClosed(t *testing.T) {
	err := framechannel.RetireSend(errors.New("closed without provider text"))
	if got := lifecycleCause(err); got != LifecycleCauseNaturalRetirement {
		t.Fatalf("retirement cause = %q", got)
	}
}
