package sessioncore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type writeClass uint8

const (
	writeClassControl writeClass = iota
	writeClassData
	writeClassTerminal
)

type writePhase uint8

const (
	writeQueued writePhase = iota
	writeInFlight
	writeDone
)

type writeEntry struct {
	class       writeClass
	kind        protocolsession.MessageKind
	plaintext   []byte
	frameSize   int
	onDone      func(deliveredOrUncertain bool)
	done        chan struct{}
	phase       writePhase
	disposition framechannel.SendDisposition
	err         error
}

type writerState uint8

const (
	writerAccepting writerState = iota
	writerTerminalAccepted
	writerStopped
)

type laneQueueUsage struct {
	frames int
	bytes  int
}

type envelopeSealer interface {
	Seal(plaintext []byte) (protocolsession.SealedEnvelope, error)
}

// laneWriter serializes AEAD sealing and FrameChannel writes for one lane with
// strict priority (terminal > control > data) and bounded burst fairness.
// Physical sends always execute on the lane's lifetime context so caller
// cancellation can withdraw queued frames cleanly without ever aborting an
// already-sealed AEAD sequence number.
type laneWriter struct {
	channel protocolsession.FrameChannel
	sealer  envelopeSealer
	wake    chan struct{}
	done    chan struct{}
	started atomic.Bool

	mu           sync.Mutex
	state        writerState
	runErr       error
	terminal     *writeEntry
	control      []*writeEntry
	data         []*writeEntry
	controlUsage laneQueueUsage
	dataUsage    laneQueueUsage
	controlBurst int
	dataBurst    int
}

func newLaneWriter(
	channel protocolsession.FrameChannel,
	sealer envelopeSealer,
) (*laneWriter, error) {
	if channel == nil || sealer == nil {
		return nil, protocolsession.ErrNilRuntimeDependency
	}
	return &laneWriter{
		channel: channel,
		sealer:  sealer,
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}, nil
}

func (w *laneWriter) Done() <-chan struct{} { return w.done }

func (w *laneWriter) Accepting() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state == writerAccepting
}

func (w *laneWriter) SendControl(
	ctx context.Context,
	message protocolsession.Message,
	onDone func(deliveredOrUncertain bool),
) error {
	if message.IsData() || message.Kind() == protocolsession.MessageSessionTerminal {
		if onDone != nil {
			onDone(false)
		}
		return protocolsession.MarkNotSent(protocolsession.ErrMessageClass)
	}
	return w.send(ctx, writeClassControl, message, onDone)
}

func (w *laneWriter) SendData(
	ctx context.Context,
	message protocolsession.Message,
	onDone func(deliveredOrUncertain bool),
) error {
	if !message.IsData() {
		if onDone != nil {
			onDone(false)
		}
		return protocolsession.MarkNotSent(protocolsession.ErrMessageClass)
	}
	return w.send(ctx, writeClassData, message, onDone)
}

// EnqueueControl queues a control message without waiting for physical delivery
// (used for best-effort CANCEL and offer replay ahead of a migrated candidate).
func (w *laneWriter) EnqueueControl(
	message protocolsession.Message,
	onDone func(deliveredOrUncertain bool),
) error {
	if message.IsData() || message.Kind() == protocolsession.MessageSessionTerminal {
		if onDone != nil {
			onDone(false)
		}
		return protocolsession.MarkNotSent(protocolsession.ErrMessageClass)
	}
	_, err := w.enqueue(writeClassControl, message, onDone)
	if err != nil && onDone != nil {
		onDone(false)
	}
	return err
}

type terminalSend struct {
	entry *writeEntry
}

type terminalSendResult struct {
	settled     bool
	disposition framechannel.SendDisposition
	err         error
}

func (t *terminalSend) Await(ctx context.Context) terminalSendResult {
	if t == nil || t.entry == nil {
		return terminalSendResult{settled: true, err: protocolsession.MarkNotSent(protocolsession.ErrWriterStopped)}
	}
	select {
	case <-t.entry.done:
		return terminalSendResult{settled: true, disposition: t.entry.disposition, err: t.entry.err}
	default:
	}
	select {
	case <-t.entry.done:
		return terminalSendResult{settled: true, disposition: t.entry.disposition, err: t.entry.err}
	case <-ctx.Done():
		return terminalSendResult{settled: false, err: ctx.Err()}
	}
}

// EnqueueTerminal preempts all queued control/data frames on this lane.
func (w *laneWriter) EnqueueTerminal(
	message protocolsession.Message,
	onAccept func() error,
) (*terminalSend, error) {
	if message.Kind() != protocolsession.MessageSessionTerminal {
		return nil, protocolsession.MarkNotSent(protocolsession.ErrMessageClass)
	}
	entry, err := prepareWriteEntry(writeClassTerminal, message, nil)
	if err != nil {
		return nil, protocolsession.MarkNotSent(err)
	}
	w.mu.Lock()
	if w.state == writerTerminalAccepted {
		w.mu.Unlock()
		return nil, protocolsession.MarkNotSent(protocolsession.ErrWriterTerminal)
	}
	if w.state == writerStopped {
		w.mu.Unlock()
		return nil, protocolsession.MarkNotSent(protocolsession.ErrWriterStopped)
	}
	if onAccept != nil {
		if err := onAccept(); err != nil {
			w.mu.Unlock()
			return nil, protocolsession.MarkNotSent(err)
		}
	}
	w.state = writerTerminalAccepted
	drainedControl := w.control
	drainedData := w.data
	w.control = nil
	w.data = nil
	w.controlUsage = laneQueueUsage{}
	w.dataUsage = laneQueueUsage{}
	w.terminal = entry
	terminalErr := protocolsession.MarkNotSent(protocolsession.ErrWriterTerminal)
	callbacks := make([]func(bool), 0, len(drainedControl)+len(drainedData))
	callbacks = finishEntriesLocked(callbacks, drainedControl, 0, terminalErr)
	callbacks = finishEntriesLocked(callbacks, drainedData, 0, terminalErr)
	w.mu.Unlock()
	for _, fn := range callbacks {
		fn(false)
	}
	w.signal()
	return &terminalSend{entry: entry}, nil
}

func (w *laneWriter) send(
	ctx context.Context,
	class writeClass,
	message protocolsession.Message,
	onDone func(deliveredOrUncertain bool),
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		if onDone != nil {
			onDone(false)
		}
		return protocolsession.MarkNotSent(err)
	}
	entry, err := w.enqueue(class, message, onDone)
	if err != nil {
		if onDone != nil {
			onDone(false)
		}
		return err
	}
	select {
	case <-entry.done:
		return entry.err
	case <-ctx.Done():
		return w.cancelQueuedOrLeaveInFlight(entry, ctx.Err())
	}
}

func (w *laneWriter) enqueue(
	class writeClass,
	message protocolsession.Message,
	onDone func(deliveredOrUncertain bool),
) (*writeEntry, error) {
	entry, err := prepareWriteEntry(class, message, onDone)
	if err != nil {
		return nil, protocolsession.MarkNotSent(err)
	}
	w.mu.Lock()
	if w.state == writerTerminalAccepted {
		w.mu.Unlock()
		return nil, protocolsession.MarkNotSent(protocolsession.ErrWriterTerminal)
	}
	if w.state == writerStopped {
		w.mu.Unlock()
		return nil, protocolsession.MarkNotSent(protocolsession.ErrWriterStopped)
	}
	if class == writeClassData {
		if w.dataUsage.frames >= protocolsession.DataQueueFrameLimit ||
			entry.frameSize > protocolsession.DataQueueByteLimit-w.dataUsage.bytes {
			w.mu.Unlock()
			return nil, protocolsession.MarkNotSent(protocolsession.ErrDataQueueFull)
		}
		w.dataUsage.frames++
		w.dataUsage.bytes += entry.frameSize
		w.data = append(w.data, entry)
	} else {
		if w.controlUsage.frames >= protocolsession.ControlQueueFrameLimit ||
			entry.frameSize > protocolsession.ControlQueueByteLimit-w.controlUsage.bytes {
			w.mu.Unlock()
			return nil, protocolsession.MarkNotSent(protocolsession.ErrControlQueueFull)
		}
		w.controlUsage.frames++
		w.controlUsage.bytes += entry.frameSize
		w.control = append(w.control, entry)
	}
	w.mu.Unlock()
	w.signal()
	return entry, nil
}

func (w *laneWriter) cancelQueuedOrLeaveInFlight(entry *writeEntry, cause error) error {
	w.mu.Lock()
	switch entry.phase {
	case writeDone:
		err := entry.err
		w.mu.Unlock()
		return err
	case writeQueued:
		if entry.class == writeClassData {
			w.data = removeWriteEntry(w.data, entry)
			w.dataUsage.frames--
			w.dataUsage.bytes -= entry.frameSize
		} else {
			w.control = removeWriteEntry(w.control, entry)
			w.controlUsage.frames--
			w.controlUsage.bytes -= entry.frameSize
		}
		notSentErr := protocolsession.MarkNotSent(cause)
		onDone := finishEntryLocked(entry, 0, notSentErr)
		w.mu.Unlock()
		if onDone != nil {
			onDone(false)
		}
		return notSentErr
	default:
		// Already in flight on laneCtx: let the physical send finish so AEAD
		// sequence monotonicity is preserved; caller cannot prove non-delivery.
		w.mu.Unlock()
		return cause
	}
}

func (w *laneWriter) Run(ctx context.Context) error {
	if !w.started.CompareAndSwap(false, true) {
		return protocolsession.ErrWriterStopped
	}
	err := w.runLoop(ctx)
	w.stop(err)
	return err
}

func (w *laneWriter) runLoop(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry := w.popNext()
		if entry == nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-w.wake:
				continue
			}
		}
		err := w.deliver(ctx, entry)
		if err != nil || entry.class == writeClassTerminal {
			return err
		}
	}
}

func (w *laneWriter) popNext() *writeEntry {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.terminal != nil {
		entry := w.terminal
		w.terminal = nil
		entry.phase = writeInFlight
		return entry
	}
	popControl := func() *writeEntry {
		if len(w.control) == 0 {
			return nil
		}
		entry := w.control[0]
		w.control[0] = nil
		w.control = w.control[1:]
		if len(w.control) == 0 {
			w.control = nil
		}
		w.controlUsage.frames--
		w.controlUsage.bytes -= entry.frameSize
		w.dataBurst = 0
		w.controlBurst++
		entry.phase = writeInFlight
		return entry
	}
	popData := func() *writeEntry {
		if len(w.data) == 0 {
			return nil
		}
		entry := w.data[0]
		w.data[0] = nil
		w.data = w.data[1:]
		if len(w.data) == 0 {
			w.data = nil
		}
		w.dataUsage.frames--
		w.dataUsage.bytes -= entry.frameSize
		w.dataBurst++
		w.controlBurst = 0
		entry.phase = writeInFlight
		return entry
	}
	if w.controlBurst >= protocolsession.MaximumControlBurst {
		if entry := popData(); entry != nil {
			return entry
		}
	}
	if w.dataBurst == 0 || w.dataBurst >= protocolsession.MaximumDataBurst {
		if entry := popControl(); entry != nil {
			return entry
		}
		w.dataBurst = 0
	}
	if w.dataBurst > 0 {
		if entry := popData(); entry != nil {
			return entry
		}
	}
	if entry := popControl(); entry != nil {
		return entry
	}
	return popData()
}

func (w *laneWriter) deliver(ctx context.Context, entry *writeEntry) error {
	sealed, err := w.sealer.Seal(entry.plaintext)
	if err != nil {
		wrapped := protocolsession.MarkNotSent(fmt.Errorf("write message kind %d: %w", entry.kind, err))
		w.finishEntry(entry, 0, wrapped)
		return wrapped
	}
	sendErr := w.sendFrame(ctx, entry.class, framechannel.Frame(sealed.Frame))
	disposition := framechannel.SendDispositionOf(sendErr)
	if sendErr == nil {
		w.finishEntry(entry, framechannel.SendAccepted, nil)
		return nil
	}
	wrapped := fmt.Errorf("write message kind %d: %w", entry.kind, sendErr)
	if disposition != framechannel.SendAccepted {
		wrapped = protocolsession.MarkNotSent(wrapped)
	}
	w.finishEntry(entry, disposition, wrapped)
	return wrapped
}

func (w *laneWriter) finishEntry(
	entry *writeEntry,
	disposition framechannel.SendDisposition,
	err error,
) {
	w.mu.Lock()
	onDone := finishEntryLocked(entry, disposition, err)
	w.mu.Unlock()
	if onDone != nil {
		onDone(err == nil || !protocolsession.IsNotSent(err))
	}
}

func (w *laneWriter) sendFrame(
	ctx context.Context,
	class writeClass,
	frame framechannel.Frame,
) error {
	if class == writeClassTerminal {
		return w.channel.SendTerminal(ctx, frame)
	}
	if class == writeClassControl {
		if controlChannel, ok := w.channel.(interface {
			SendControl(context.Context, framechannel.Frame) error
		}); ok {
			return controlChannel.SendControl(ctx, frame)
		}
	}
	return w.channel.Send(ctx, frame)
}

func (w *laneWriter) stop(runErr error) {
	cause := protocolsession.ErrWriterStopped
	if runErr != nil {
		cause = errors.Join(protocolsession.ErrWriterStopped, runErr)
	}
	notSentCause := protocolsession.MarkNotSent(cause)

	w.mu.Lock()
	w.state = writerStopped
	w.runErr = runErr
	drainedControl := w.control
	drainedData := w.data
	drainedTerminal := w.terminal
	w.control = nil
	w.data = nil
	w.terminal = nil
	w.controlUsage = laneQueueUsage{}
	w.dataUsage = laneQueueUsage{}
	callbacks := make([]func(bool), 0, len(drainedControl)+len(drainedData)+1)
	callbacks = finishEntriesLocked(callbacks, drainedControl, 0, notSentCause)
	callbacks = finishEntriesLocked(callbacks, drainedData, 0, notSentCause)
	if drainedTerminal != nil {
		if fn := finishEntryLocked(drainedTerminal, 0, notSentCause); fn != nil {
			callbacks = append(callbacks, fn)
		}
	}
	close(w.done)
	w.mu.Unlock()
	for _, fn := range callbacks {
		fn(false)
	}
}

func (w *laneWriter) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func finishEntryLocked(
	entry *writeEntry,
	disposition framechannel.SendDisposition,
	err error,
) func(bool) {
	if entry.phase == writeDone {
		return nil
	}
	entry.phase = writeDone
	entry.disposition = disposition
	entry.err = err
	onDone := entry.onDone
	entry.onDone = nil
	close(entry.done)
	return onDone
}

func finishEntriesLocked(
	callbacks []func(bool),
	entries []*writeEntry,
	disposition framechannel.SendDisposition,
	err error,
) []func(bool) {
	for _, entry := range entries {
		if fn := finishEntryLocked(entry, disposition, err); fn != nil {
			callbacks = append(callbacks, fn)
		}
	}
	return callbacks
}

func prepareWriteEntry(
	class writeClass,
	message protocolsession.Message,
	onDone func(deliveredOrUncertain bool),
) (*writeEntry, error) {
	plaintext, err := protocolsession.EncodeMessage(message)
	if err != nil {
		return nil, err
	}
	frameSize, err := protocolsession.EnvelopeFrameSize(len(plaintext))
	if err != nil {
		return nil, err
	}
	return &writeEntry{
		class:     class,
		kind:      message.Kind(),
		plaintext: plaintext,
		frameSize: frameSize,
		onDone:    onDone,
		done:      make(chan struct{}),
		phase:     writeQueued,
	}, nil
}

func removeWriteEntry(queue []*writeEntry, target *writeEntry) []*writeEntry {
	for i, entry := range queue {
		if entry == target {
			copy(queue[i:], queue[i+1:])
			queue[len(queue)-1] = nil
			queue = queue[:len(queue)-1]
			if len(queue) == 0 {
				return nil
			}
			return queue
		}
	}
	return queue
}
