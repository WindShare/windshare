package protocolsession

import (
	"context"
	"errors"
	"fmt"

	framechannel "github.com/windshare/windshare/core/framechannel"
)

// Run is the only code path that calls Send or SendTerminal. A second call is
// rejected before it can create a competing transport writer.
func (writer *SessionWriter) Run(ctx context.Context) error {
	if !writer.started.CompareAndSwap(false, true) {
		return ErrWriterReused
	}
	err := writer.run(ctx)
	writer.stop(err)
	return err
}

func (writer *SessionWriter) run(ctx context.Context) error {
	schedule := writerSchedule{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		item, terminal := writer.tryScheduled(&schedule)
		if item == nil {
			var err error
			terminal, err = writer.waitAndDeliver(ctx, &schedule)
			if err != nil || terminal {
				return err
			}
			continue
		}
		if err := writer.deliver(ctx, item); err != nil || terminal {
			return err
		}
	}
}

type writerSchedule struct {
	dataBurst    int
	controlBurst int
}

func (writer *SessionWriter) tryScheduled(schedule *writerSchedule) (*queuedMessage, bool) {
	if terminal := writer.tryTakeTerminal(); terminal != nil {
		return terminal, true
	}
	if schedule.controlBurst >= MaximumControlBurst {
		if data := writer.tryTakeData(); data != nil {
			schedule.dataBurst = 1
			schedule.controlBurst = 0
			return data, false
		}
	}
	if schedule.dataBurst == 0 || schedule.dataBurst >= MaximumDataBurst {
		if control := writer.tryTakeControl(); control != nil {
			schedule.dataBurst = 0
			schedule.controlBurst++
			return control, false
		}
		schedule.dataBurst = 0
	}
	if schedule.dataBurst > 0 {
		if data := writer.tryTakeData(); data != nil {
			schedule.dataBurst++
			schedule.controlBurst = 0
			return data, false
		}
	}
	if control := writer.tryTakeControl(); control != nil {
		schedule.dataBurst = 0
		schedule.controlBurst++
		return control, false
	}
	if data := writer.tryTakeData(); data != nil {
		schedule.dataBurst = 1
		schedule.controlBurst = 0
		return data, false
	}
	return nil, false
}

func (writer *SessionWriter) waitAndDeliver(ctx context.Context, schedule *writerSchedule) (bool, error) {
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case terminal := <-writer.terminal:
		return true, writer.deliver(ctx, terminal)
	case control := <-writer.control:
		writer.releaseUsage(&writer.controlUsage, control)
		schedule.dataBurst = 0
		schedule.controlBurst++
		return false, writer.deliver(ctx, control)
	case data := <-writer.data:
		writer.releaseUsage(&writer.dataUsage, data)
		schedule.dataBurst = 1
		schedule.controlBurst = 0
		return false, writer.deliver(ctx, data)
	}
}

func (writer *SessionWriter) tryTakeTerminal() *queuedMessage {
	select {
	case message := <-writer.terminal:
		return message
	default:
		return nil
	}
}

func (writer *SessionWriter) tryTakeControl() *queuedMessage {
	select {
	case message := <-writer.control:
		writer.releaseUsage(&writer.controlUsage, message)
		return message
	default:
		return nil
	}
}

func (writer *SessionWriter) tryTakeData() *queuedMessage {
	select {
	case message := <-writer.data:
		writer.releaseUsage(&writer.dataUsage, message)
		return message
	default:
		return nil
	}
}

func (writer *SessionWriter) releaseUsage(usage *queueUsage, message *queuedMessage) {
	writer.mu.Lock()
	usage.frames--
	usage.bytes -= message.frameSize
	writer.mu.Unlock()
}

func (writer *SessionWriter) stop(runErr error) {
	writer.mu.Lock()
	writer.state = writerStopped
	writer.runErr = runErr
	writer.failQueuedLocked(writer.control, &writer.controlUsage, stoppedCause(runErr))
	writer.failQueuedLocked(writer.data, &writer.dataUsage, stoppedCause(runErr))
	select {
	case terminal := <-writer.terminal:
		terminal.result.complete(SendOutcomeDropped, terminal.replay, false, stoppedCause(runErr))
		terminal.settleContinuation(false)
		terminal.releasePin()
	default:
	}
	close(writer.done)
	writer.mu.Unlock()
}

func (writer *SessionWriter) failQueuedLocked(
	queue chan *queuedMessage,
	usage *queueUsage,
	cause error,
) {
	for {
		select {
		case message := <-queue:
			usage.frames--
			usage.bytes -= message.frameSize
			message.result.complete(SendOutcomeDropped, message.replay, true, cause)
			message.settleContinuation(false)
			message.releasePin()
		default:
			return
		}
	}
}

func stoppedCause(runErr error) error {
	if runErr == nil {
		return ErrWriterStopped
	}
	return errors.Join(ErrWriterStopped, runErr)
}

func (writer *SessionWriter) Done() <-chan struct{} { return writer.done }

// Accepting reports whether this writer can still acquire a new message
// lifecycle. It closes the gap where terminal acceptance or Run completion has
// changed writer authority before an owning lane publishes its closing state.
func (writer *SessionWriter) Accepting() bool {
	if writer == nil {
		return false
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.state == writerAccepting
}

func (writer *SessionWriter) Err() error {
	select {
	case <-writer.done:
		writer.mu.Lock()
		defer writer.mu.Unlock()
		return writer.runErr
	default:
		return nil
	}
}

func (writer *SessionWriter) deliver(ctx context.Context, item *queuedMessage) error {
	claimed, policyPrepared, alreadyAdmitted := item.result.claim()
	if !claimed {
		// Await may retract an item after it leaves the channel but before policy
		// admission. The writer still owns queue accounting, but no wire work.
		return nil
	}
	if policyPrepared {
		defer item.releasePin()
	}
	if !writer.acceptsClaimedDelivery(item) {
		return nil
	}

	sequence, err := writer.prepareSequencedDelivery(item)
	if err != nil {
		return item.completePreparationFailure(err)
	}
	permit := item.replay
	if item.class != classTerminal && !policyPrepared {
		var proceed bool
		permit, proceed, err = writer.admitClaimedDelivery(item, permit)
		if err != nil || !proceed {
			return err
		}
		defer item.releasePin()
	}
	if policyPrepared && !alreadyAdmitted {
		if !item.result.beginReservationSeal() {
			item.settleContinuation(false)
			return nil
		}
	}

	sealed, err := writer.sealDelivery(item, sequence)
	if err == nil && policyPrepared && !alreadyAdmitted {
		if commitErr := item.result.commitReservationSeal(); commitErr != nil {
			// Losing this state after Seal means the sequence may already be
			// consumed. Returning the invariant error fail-stops this writer.
			err = commitErr
		} else {
			// The result transition committed the shared reservation before exposing
			// replay authority; the item must not try to settle that alias again.
			item.continuation = nil
		}
	}
	var transportDisposition framechannel.SendDisposition
	if err == nil {
		err = writer.sendSealed(ctx, item.class, sealed)
		transportDisposition = framechannel.SendDispositionOf(err)
	}
	return item.completePhysicalDelivery(permit, transportDisposition, err)
}

func (writer *SessionWriter) acceptsClaimedDelivery(item *queuedMessage) bool {
	writer.mu.Lock()
	accepted := item.class == classTerminal || writer.state == writerAccepting
	writer.mu.Unlock()
	if accepted {
		return true
	}
	item.settleContinuation(false)
	item.result.complete(SendOutcomeDropped, item.replay, false, ErrWriterTerminal)
	return false
}

func (writer *SessionWriter) prepareSequencedDelivery(item *queuedMessage) (uint64, error) {
	sequence, err := writer.sealer.NextSequence()
	if err == nil {
		err = writer.materializeSequenced(item, sequence)
	}
	if err == nil && len(item.plaintext) != item.frameSize-EnvelopeOverheadBytes {
		err = ErrSequencedSize
	}
	if err != nil {
		return 0, fmt.Errorf("write message kind %d: %w", item.message.kind, err)
	}
	return sequence, nil
}

func (item *queuedMessage) completePreparationFailure(err error) error {
	item.settleContinuation(false)
	if !item.result.complete(SendOutcomeDropped, item.replay, true, err) {
		return nil
	}
	return err
}

func (writer *SessionWriter) admitClaimedDelivery(
	item *queuedMessage,
	permit OutboundReplayPermit,
) (OutboundReplayPermit, bool, error) {
	decision, owned := item.result.admit(func() deliveryAdmission {
		return writer.decideDeliveryAdmission(item, permit)
	})
	if !owned {
		return permit, false, nil
	}
	permit = decision.replay
	if decision.err != nil {
		return permit, false, decision.err
	}
	if decision.disposition == OperationDrop {
		return permit, false, nil
	}
	item.pin = decision.pin
	item.continuation = decision.continuation
	return permit, true, nil
}

func (writer *SessionWriter) decideDeliveryAdmission(
	item *queuedMessage,
	permit OutboundReplayPermit,
) deliveryAdmission {
	admission := deliveryAdmission{replay: permit}
	var observed OutboundAdmission
	if permit.authority != nil {
		observed, admission.err = writer.policy.AcceptOutboundReplay(item.message, permit)
	} else {
		observed, admission.err = writer.policy.AdmitOutbound(item.message, item.authority)
	}
	admission.disposition = observed.Disposition
	admission.generation = observed.Generation
	admission.operation = observed.Operation
	admission.replay = observed.Replay
	admission.pin = observed.pin
	admission.continuation = observed.continuation
	admission.admitted = admission.err == nil && admission.disposition == OperationDeliver
	if admission.admitted && item.authenticatedViolationHandler != nil {
		if err := admission.generation.RegisterAuthenticatedOperationViolationHandler(
			item.authenticatedViolationHandler,
		); err != nil {
			// Policy already created this generation, but no peer byte has been
			// exposed. Release writer-owned aliases and report an admitted local
			// contract failure so the RPC owner can retire the exact generation.
			admission.continuation.rollback()
			admission.continuation = nil
			admission.pin.release()
			admission.pin = nil
			admission.err = err
		}
	}
	if admission.err == nil && admission.disposition == OperationDeliver &&
		writer.policy.OutboundDirection() == DirectionSenderToReceiver && admission.replay.IsZero() {
		admission.err = ErrOutboundReplayPermit
	}
	if admission.err == nil && admission.disposition != OperationDeliver &&
		admission.disposition != OperationDrop {
		admission.err = ErrMessageClass
	}
	if admission.err != nil {
		admission.err = fmt.Errorf("write message kind %d: %w", item.message.kind, admission.err)
	}
	return admission
}

func (writer *SessionWriter) sealDelivery(item *queuedMessage, sequence uint64) (SealedEnvelope, error) {
	sealed, err := writer.sealer.Seal(item.plaintext)
	if err == nil && sealed.Sequence != sequence {
		err = fmt.Errorf("%w: got %d, expected %d", ErrSealerSequence, sealed.Sequence, sequence)
	}
	return sealed, err
}

func (writer *SessionWriter) sendSealed(
	ctx context.Context,
	class messageClass,
	sealed SealedEnvelope,
) error {
	frame := framechannel.Frame(sealed.Frame)
	if class == classTerminal {
		return writer.channel.SendTerminal(ctx, frame)
	}
	return writer.channel.Send(ctx, frame)
}

func (item *queuedMessage) completePhysicalDelivery(
	permit OutboundReplayPermit,
	transportDisposition framechannel.SendDisposition,
	err error,
) error {
	if err != nil {
		err = fmt.Errorf("write message kind %d: %w", item.message.kind, err)
	}
	outcome := physicalSendOutcome(transportDisposition, err)
	retryableAcrossLane := err != nil
	if outcome == SendOutcomeDelivered {
		retryableAcrossLane = false
	}
	item.settleContinuation(outcome != SendOutcomeDropped || transportDisposition == framechannel.SendAccepted)
	item.result.completeTransport(
		outcome,
		permit,
		retryableAcrossLane,
		transportDisposition,
		err,
	)
	return err
}

func physicalSendOutcome(transportDisposition framechannel.SendDisposition, err error) SendOutcome {
	if transportDisposition != framechannel.SendAccepted {
		return SendOutcomeDropped
	}
	if err != nil {
		return SendOutcomeUnknown
	}
	return SendOutcomeDelivered
}

func (writer *SessionWriter) materializeSequenced(item *queuedMessage, sequence uint64) error {
	if item.builder == nil {
		return nil
	}
	message, err := item.builder(sequence)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSequencedBuild, err)
	}
	if !messageMatchesClass(message, item.class) {
		return ErrMessageClass
	}
	plaintext, err := EncodeMessage(message)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSequencedBuild, err)
	}
	if len(plaintext) != item.frameSize-EnvelopeOverheadBytes {
		return ErrSequencedSize
	}
	item.message = message
	item.plaintext = plaintext
	return nil
}

func messageMatchesClass(message Message, class messageClass) bool {
	switch class {
	case classControl:
		return !message.IsData() && message.kind != MessageSessionTerminal
	case classData:
		return message.IsData()
	case classTerminal:
		return message.kind == MessageSessionTerminal
	default:
		return false
	}
}
