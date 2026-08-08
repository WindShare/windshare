package v2peer

import (
	"context"
	"errors"

	"github.com/fxamacker/cbor/v2"
	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/v2signal"
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
	if attempt.admissionPhase != attemptAdmissionIdle {
		return nil, false
	}
	admissionContext, cancel := context.WithCancelCause(parent)
	attempt.admissionCancel = cancel
	attempt.admissionPhase = attemptAdmissionPending
	return admissionContext, true
}

func (attempt *peerAttempt) resolveAdmission(lane sessionruntime.LaneIdentity, err error) {
	attempt.admissionMu.Lock()
	if attempt.admissionPhase != attemptAdmissionPending {
		attempt.admissionMu.Unlock()
		return
	}
	attempt.admissionResult = attemptEvent{kind: attemptAdmission, lane: lane, err: err}
	attempt.admissionPhase = attemptAdmissionResolved
	cancel := attempt.admissionCancel
	attempt.admissionCancel = nil
	close(attempt.admissionDone)
	attempt.admissionMu.Unlock()
	if cancel != nil {
		cancel(err)
	}
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

func (execution *attemptExecution) settleAdmissionAtTerminal(cause error) (bool, error) {
	result, started := execution.attempt.admissionAtTerminal(cause)
	if !started {
		return false, nil
	}
	execution.attempt.recorder.complete(
		SenderAttemptDataChannelOpen,
		execution.candidateCounts(),
		nil,
		nil,
	)
	if err := execution.acceptAdmission(result); err != nil {
		return false, err
	}
	return true, nil
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
	if err := rejectedOfferIdentityDecoding.Unmarshal(encoded, &fields); err != nil || len(fields) < 3 {
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
	execution.channel = channel
	admissionEventsComplete := make(chan struct{})
	execution.children.Add(2)
	go func() {
		defer execution.children.Done()
		defer close(admissionEventsComplete)
		execution.attempt.awaitOpenAndAdmit(execution.ctx, channel)
	}()
	go func() {
		defer execution.children.Done()
		execution.attempt.watchChannel(channel, admissionEventsComplete)
	}()
	return nil
}

func (execution *attemptExecution) acceptAdmission(event attemptEvent) error {
	if event.err != nil {
		return errors.Join(errChannelAdmission, event.err)
	}
	if event.lane.ID == 0 || event.lane.Epoch == 0 {
		return errors.Join(errChannelAdmission, errors.New("peer DataChannel admission returned a zero lane"))
	}
	execution.attempt.attached.Store(true)
	execution.stopDeadline()
	execution.attempt.recorder.complete(
		SenderAttemptLaneAdmissionStarted, execution.candidateCounts(), &event.lane, nil,
	)
	pair := selectedPairEvidence(execution.peer)
	execution.attempt.recorder.complete(
		SenderAttemptAdmitted, execution.candidateCounts(), &event.lane, pair,
	)
	return nil
}

func (attempt *peerAttempt) awaitOpenAndAdmit(ctx context.Context, channel PeerDataChannel) {
	if !awaitDataChannelOpen(ctx, channel) {
		return
	}
	admissionContext, admitted := attempt.beginAdmission(ctx)
	if !admitted {
		return
	}
	// Both events come from this goroutine, so FIFO delivery makes the public
	// open milestone precede every admission result even when admission is fast.
	attempt.push(attemptEvent{kind: attemptDataChannelOpen})
	lane, err := attempt.config.session.AdmitPeerChannel(admissionContext, channel)
	attempt.resolveAdmission(lane, err)
	attempt.push(attemptEvent{kind: attemptAdmission, lane: lane, err: err})
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
