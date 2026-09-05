package v2peer

import (
	"context"
	"errors"

	"github.com/fxamacker/cbor/v2"
	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

type attemptAdmissionPhase uint8

const (
	attemptAdmissionIdle attemptAdmissionPhase = iota
	attemptAdmissionPending
	attemptAdmissionResolved
	attemptAdmissionClosed
)

// beginAdmission and admissionAtTerminal form the arbitration boundary between
// the core lane registry and transport failures. Once core starts admission, a
// competing terminal must await its result: a successful return means the lane
// is already registered and therefore owns the public attempt terminal.
func (attempt *peerAttempt) beginAdmission(
	parent context.Context,
) (context.Context, bool) {
	attempt.admissionMu.Lock()
	defer attempt.admissionMu.Unlock()
	if attempt.admissionPhase != attemptAdmissionIdle || parent == nil {
		return nil, false
	}
	admissionContext, cancel := context.WithCancelCause(parent)
	attempt.admissionCancel = cancel
	attempt.admissionPhase = attemptAdmissionPending
	return admissionContext, true
}

func (attempt *peerAttempt) resolveAdmission(
	result sessionruntime.SenderPeerAdmissionResult,
	err error,
) {
	attempt.admissionMu.Lock()
	if attempt.admissionPhase != attemptAdmissionPending {
		attempt.admissionMu.Unlock()
		return
	}
	attempt.admissionResult = attemptEvent{
		kind: attemptAdmission, lane: result.Lane, admission: result, err: err,
	}
	attempt.admissionPhase = attemptAdmissionResolved
	cancel := attempt.admissionCancel
	attempt.admissionCancel = nil
	close(attempt.admissionDone)
	attempt.admissionMu.Unlock()
	if cancel != nil {
		cancel(err)
	}
}

func (attempt *peerAttempt) authenticatedSettlementBegan() bool {
	attempt.admissionMu.Lock()
	defer attempt.admissionMu.Unlock()
	return attempt.admissionResult.admission.SettlementBegan
}

func (attempt *peerAttempt) admissionAtTerminal(cause error) (attemptEvent, bool) {
	attempt.admissionMu.Lock()
	switch attempt.admissionPhase {
	case attemptAdmissionIdle:
		attempt.admissionPhase = attemptAdmissionClosed
		attempt.admissionMu.Unlock()
		return attemptEvent{}, false
	case attemptAdmissionClosed:
		attempt.admissionMu.Unlock()
		return attemptEvent{}, false
	case attemptAdmissionResolved:
		result := attempt.admissionResult
		attempt.admissionMu.Unlock()
		return result, true
	case attemptAdmissionPending:
		done := attempt.admissionDone
		cancel := attempt.admissionCancel
		attempt.admissionMu.Unlock()
		if cause == nil {
			cause = context.Canceled
		}
		// Cancellation asks the in-flight core admission to settle; it does not
		// pre-judge that settlement. A core success that races this cancellation
		// is still an already-registered lane and must remain admitted evidence.
		if cancel != nil {
			cancel(cause)
		}
		<-done
		attempt.admissionMu.Lock()
		result := attempt.admissionResult
		attempt.admissionMu.Unlock()
		return result, true
	default:
		attempt.admissionMu.Unlock()
		return attemptEvent{}, false
	}
}

func (execution *attemptExecution) settleAdmissionAtTerminal(
	cause error,
) (bool, bool, error) {
	result, started := execution.attempt.admissionAtTerminal(cause)
	if !started {
		execution.attempt.phases.terminate(cause)
		return false, false, nil
	}
	execution.attempt.recorder.dataChannelOpened(execution.candidateCounts())
	owned, admitted, err := execution.acceptAdmission(result)
	if !owned {
		return false, false, nil
	}
	return true, admitted, err
}

var rejectedOfferIdentityDecoding = func() cbor.DecMode {
	mode, err := cbor.DecOptions{
		DupMapKey:        cbor.DupMapKeyEnforcedAPF,
		IndefLength:      cbor.IndefLengthForbidden,
		TagsMd:           cbor.TagsForbidden,
		MaxNestedLevels:  4,
		MaxArrayElements: 16,
		MaxMapPairs:      16,
	}.DecMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

// recoverOfferBinding is deliberately narrower than offer decoding. A malformed
// SDP or non-canonical tail still names the browser's evidence identity when the
// frozen version/path/attempt prefix is unambiguous, so that rejection needs its
// one terminal stream even though the offer itself must remain unusable.
func recoverOfferBinding(encoded []byte) (v2signal.Binding, bool) {
	var fields []cbor.RawMessage
	if err := rejectedOfferIdentityDecoding.Unmarshal(encoded, &fields); err != nil || len(fields) < 4 {
		return v2signal.Binding{}, false
	}
	var version uint64
	var pathBytes []byte
	var attemptBytes []byte
	if rejectedOfferIdentityDecoding.Unmarshal(fields[0], &version) != nil ||
		version != v2signal.SignalingSchemaVersion ||
		rejectedOfferIdentityDecoding.Unmarshal(fields[1], &pathBytes) != nil ||
		rejectedOfferIdentityDecoding.Unmarshal(fields[2], &attemptBytes) != nil ||
		len(pathBytes) != v2signal.IdentityBytes || len(attemptBytes) != v2signal.IdentityBytes {
		return v2signal.Binding{}, false
	}
	var binding v2signal.Binding
	copy(binding.PeerPathID[:], pathBytes)
	copy(binding.AttemptID[:], attemptBytes)
	if rejectedOfferIdentityDecoding.Unmarshal(fields[3], &binding.AttemptSequence) != nil {
		return v2signal.Binding{}, false
	}
	if binding.Validate() != nil {
		return v2signal.Binding{}, false
	}
	return binding, true
}

func (execution *attemptExecution) startDataChannel(raw *pion.DataChannel) error {
	if raw == nil {
		return errors.Join(errChannelAdmission, errors.New("peer delivered a nil DataChannel"))
	}
	if execution.dataChannelSeen {
		_ = raw.Close()
		return errors.Join(errChannelAdmission, errors.New("peer created more than one DataChannel"))
	}
	execution.dataChannelSeen = true
	channel, err := execution.attempt.config.factory.dataChannels.WrapDataChannel(raw)
	if err != nil || channel == nil {
		return errors.Join(errChannelAdmission, err)
	}
	execution.transport = newOwnedPeerDataChannel(execution.peer, channel)
	execution.channel = execution.transport
	openTransition := make(chan struct{})
	execution.openTransition = openTransition
	admissionEventsComplete := make(chan struct{})
	execution.children.Add(2)
	go func() {
		defer execution.children.Done()
		defer close(admissionEventsComplete)
		execution.attempt.awaitOpenAndAdmit(execution.ctx, execution.transport, openTransition)
	}()
	go func() {
		defer execution.children.Done()
		execution.attempt.watchChannel(execution.transport, admissionEventsComplete)
	}()
	return nil
}

func (execution *attemptExecution) acceptAdmission(
	event attemptEvent,
) (bool, bool, error) {
	result := event.admission
	admitted := event.err == nil &&
		result.Disposition == sessionruntime.SenderPeerAdmissionAccepted &&
		result.ResponseDelivery == sessionruntime.SenderPeerResponseDelivered &&
		result.LaneAttachment == sessionruntime.SenderPeerLaneAttached &&
		result.Lane.ID != 0 && result.Lane.Epoch != 0
	if !execution.attempt.phases.settleSenderAdmission(result.SettlementBegan, admitted) {
		return false, false, nil
	}
	if result.SettlementBegan {
		if result.GrantOperationID.IsZero() || result.Lane.ID == 0 || result.Lane.Epoch == 0 {
			return true, false, errors.Join(
				errChannelAdmission,
				errors.New("authenticated peer admission returned incomplete identity evidence"),
			)
		}
		execution.attempt.recorder.admissionSettled(result, execution.candidateCounts())
	}
	switch result.Disposition {
	case sessionruntime.SenderPeerAdmissionRejected:
		if result.Rejection.Code == 0 {
			return true, false, errors.Join(
				errChannelAdmission,
				errors.New("authenticated peer admission omitted its rejection"),
				event.err,
			)
		}
		return true, false, errors.Join(
			&sessionruntime.LaneRejectedError{Rejection: result.Rejection},
			event.err,
		)
	case sessionruntime.SenderPeerAdmissionAccepted:
		if !admitted {
			return true, false, errors.Join(errChannelAdmission, event.err)
		}
	case sessionruntime.SenderPeerAdmissionSilentClose:
		if result.SettlementBegan {
			return true, false, errors.Join(
				errChannelAdmission,
				errors.New("authenticated peer admission returned a silent disposition"),
			)
		}
		return true, false, errors.Join(errChannelAdmission, event.err)
	default:
		return true, false, errors.Join(
			errChannelAdmission,
			errors.New("peer admission returned an unknown disposition"),
			event.err,
		)
	}
	execution.attempt.attached.Store(true)
	execution.attempt.config.factory.native.SetDirect([16]byte(execution.attempt.config.session.ProtocolSessionID()), execution.attempt.binding().PeerPathID)
	execution.attempt.recorder.complete(
		SenderAttemptAdmitted, execution.candidateCounts(), SenderAttemptObservation{
			Phase:                SenderAttemptPhaseAdmission,
			GrantOperationID:     result.GrantOperationID,
			Lane:                 &result.Lane,
			AdmissionDisposition: SenderAdmissionAccepted,
			ResponseDelivery:     SenderResponseDelivered,
		},
	)
	return true, true, nil
}

func (attempt *peerAttempt) awaitOpenAndAdmit(
	ctx context.Context,
	channel *ownedPeerDataChannel,
	openTransition chan<- struct{},
) {
	if !awaitDataChannelOpen(ctx, channel) {
		return
	}
	phaseContext, transitioned, err := attempt.phases.beginAdmission(ctx)
	if err != nil {
		close(openTransition)
		attempt.push(attemptEvent{kind: attemptDataChannelOpen, err: err})
		return
	}
	if !transitioned {
		close(openTransition)
		return
	}
	admissionContext, admitted := attempt.beginAdmission(phaseContext)
	if !admitted {
		close(openTransition)
		return
	}
	close(openTransition)
	// One goroutine owns Open and admission publication, so the public phase
	// transition remains ordered even when authenticated settlement is immediate.
	attempt.push(attemptEvent{
		kind: attemptDataChannelOpen, admissionContext: phaseContext,
	})
	if !channel.consume() {
		attempt.resolveAdmission(sessionruntime.SenderPeerAdmissionResult{}, ErrProtocol)
		return
	}
	result, err := attempt.config.session.AdmitPeerChannel(
		admissionContext,
		channel,
		sessionruntime.SenderPeerAdmissionControlFunc(func(
			operationID protocolsession.OperationID,
			lane sessionruntime.LaneIdentity,
		) bool {
			if !attempt.phases.beginAuthenticatedSettlement() {
				return false
			}
			attempt.recorder.laneHelloAuthenticated(operationID, lane)
			return true
		}),
	)
	attempt.resolveAdmission(result, err)
	attempt.push(attemptEvent{
		kind: attemptAdmission, lane: result.Lane, admission: result, err: err,
	})
}

func awaitDataChannelOpen(ctx context.Context, channel PeerDataChannel) bool {
	select {
	case <-channel.Opened():
		return true
	default:
	}

	select {
	case <-channel.Opened():
		return true
	case <-ctx.Done():
	case <-channel.Done():
	}

	// Pion may publish Opened and Done in the same scheduler turn. Open is the
	// authoritative admission precondition, so a completed Opened signal wins
	// over teardown when both are observable.
	select {
	case <-channel.Opened():
		return true
	default:
		return false
	}
}

func (attempt *peerAttempt) watchChannel(
	channel PeerDataChannel,
	admissionEventsComplete <-chan struct{},
) {
	<-channel.Done()
	// Admission owns the terminal decision once it returns a lane. Serializing
	// Done behind its queued result prevents a fast normal close from overtaking
	// the authoritative admission event in the attempt inbox.
	<-admissionEventsComplete
	attempt.push(attemptEvent{kind: attemptChannelDone, err: channel.Err()})
}
