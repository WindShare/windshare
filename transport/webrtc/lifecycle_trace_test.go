package webrtc

import (
	"context"
	"errors"
	"reflect"
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

func TestWebRTCLifecycleDisabledHasNilStreamAndNoSource(t *testing.T) {
	fake := newFakeDataChannel(pion.DataChannelStateOpen)
	channel, err := newChannelWithRuntime(fake, defaultFlowControl, channelRuntime{})
	if err != nil {
		t.Fatalf("construct channel: %v", err)
	}
	if channel.traces != nil || channel.lifecycle.traces != nil {
		t.Fatal("disabled lifecycle stream allocated a source")
	}
	if channel.LifecycleTrace() != nil {
		t.Fatal("disabled lifecycle stream is non-nil")
	}
	if err := channel.Close(); err != nil {
		t.Fatalf("close channel: %v", err)
	}
	if completion := channel.CompleteObservations(); completion != (LifecycleObservationCompletion{}) {
		t.Fatalf("disabled completion = %+v", completion)
	}
	var nilChannel *Channel
	if nilChannel.LifecycleTrace() != nil ||
		nilChannel.CompleteObservations() != (LifecycleObservationCompletion{}) {
		t.Fatal("nil channel exposed lifecycle observations")
	}
}

func TestWebRTCLifecycleSourcePublishesCumulativeDropSummary(t *testing.T) {
	event := func(transition LifecycleTransition) LifecycleTrace {
		return channelLifecycleTrace(
			41, transition, framechannel.Closed, LifecycleTerminalNone, nil,
		)
	}

	t.Run("summary precedes the post-gap event", func(t *testing.T) {
		source, stream, err := newLifecycleTraceSource(2)
		if err != nil {
			t.Fatalf("construct lifecycle source: %v", err)
		}
		source.emit(event(LifecycleTransitionTerminationPending))
		source.emit(event(LifecycleTransitionTerminationPending))
		source.emit(event(LifecycleTransitionTerminationPending))
		<-stream
		<-stream
		source.emit(event(LifecycleTransitionClosedClean))

		summary := <-stream
		final := <-stream
		if summary.Transition != LifecycleTransitionTraceDropped || summary.Dropped != 1 ||
			final.Transition != LifecycleTransitionClosedClean {
			t.Fatalf("post-gap ordering = summary:%+v final:%+v", summary, final)
		}
		completion := source.complete()
		if _, open := <-stream; open {
			t.Fatal("completed lifecycle source remained open")
		}
		if completion.Enqueued != 4 || completion.Loss.CapacityDropped != 1 {
			t.Fatalf("drop completion = %+v", completion)
		}
	})

	t.Run("a full synthetic notice does not inflate loss", func(t *testing.T) {
		source, stream, err := newLifecycleTraceSource(1)
		if err != nil {
			t.Fatalf("construct lifecycle source: %v", err)
		}
		source.emit(event(LifecycleTransitionTerminationPending))
		source.emit(event(LifecycleTransitionTerminationPending))
		completion := source.complete()
		if completion.Enqueued != 1 || completion.Loss.CapacityDropped != 1 {
			t.Fatalf("recursive synthetic loss = %+v", completion)
		}
		<-stream
		if _, open := <-stream; open {
			t.Fatal("completed lifecycle source remained open")
		}
	})
}

func TestWebRTCLifecycleRefusalTracesCoverProducerMatrix(t *testing.T) {
	tests := []struct {
		name       string
		operation  sendOperation
		state      framechannel.ChannelState
		terminal   LifecycleTerminalState
		err        error
		transition LifecycleTransition
		disp       framechannel.SendDisposition
	}{
		{"ordinary connecting rejection", sendOrdinary, framechannel.Connecting, LifecycleTerminalNone, framechannel.RejectSend(ErrChannelNotOpen), LifecycleTransitionSendRejected, framechannel.SendRejected},
		{"terminal connecting rejection", sendTerminal, framechannel.Connecting, LifecycleTerminalNone, framechannel.RejectSend(ErrChannelNotOpen), LifecycleTransitionSendRejected, framechannel.SendRejected},
		{"ordinary closed retirement", sendOrdinary, framechannel.Closed, LifecycleTerminalNone, framechannel.RetireSend(ErrChannelClosed), LifecycleTransitionSendRetired, framechannel.SendRetired},
		{"terminal closed rejection", sendTerminal, framechannel.Closed, LifecycleTerminalLocalPending, framechannel.RejectSend(ErrTransport), LifecycleTransitionSendRejected, framechannel.SendRejected},
		{"ordinary local-terminal retirement", sendOrdinary, framechannel.Open, LifecycleTerminalLocalPending, framechannel.RetireSend(ErrChannelClosed), LifecycleTransitionSendRetired, framechannel.SendRetired},
		{"terminal remote-terminal retirement", sendTerminal, framechannel.Open, LifecycleTerminalRemotePending, framechannel.RetireSend(ErrChannelClosed), LifecycleTransitionSendRetired, framechannel.SendRetired},
		{"terminal open rejection", sendTerminal, framechannel.Open, LifecycleTerminalNone, framechannel.RejectSend(context.Canceled), LifecycleTransitionSendRejected, framechannel.SendRejected},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			trace, emitted := sendResolutionLifecycleTrace(
				21,
				&sendAdmission{id: uint64(index + 1), operation: test.operation, state: sendAdmissionRefused, err: test.err},
				test.state,
				test.terminal,
			)
			if !emitted || trace.Transition != test.transition || trace.Disposition != test.disp ||
				trace.State != test.state || trace.Terminal != test.terminal || trace.Cause == LifecycleCauseNone {
				t.Fatalf("refusal trace = %+v emitted=%t", trace, emitted)
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
