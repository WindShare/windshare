package connectivity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/transport/relay"
	transportwebrtc "github.com/windshare/windshare/transport/webrtc"
)

// SendErrorDisposition is domain policy, not CLI exit policy. A path-local
// outcome may end one transport or receiver; a share-fatal outcome invalidates
// the shared source/sealer and therefore every receiver.
type SendErrorDisposition uint8

const (
	SendPathEnded SendErrorDisposition = iota
	SendShareFatal
)

type SendErrorClassifier func(error) SendErrorDisposition

// ClassifySendError preserves the original domain branch when terminal delivery
// contributes a diagnostic error. Transport joins must not hide source drift or
// an unknown sealer failure shared by every receiver, while a recognized
// path-local sentinel is definitive even when it wraps unknown diagnostics.
func ClassifySendError(err error) SendErrorDisposition {
	if err == nil {
		return SendPathEnded
	}
	if errors.Is(err, osfs.ErrDrift) {
		return SendShareFatal
	}
	if errors.Is(err, session.ErrTerminalDelivery) {
		joined, ok := err.(interface{ Unwrap() []error })
		if !ok {
			return SendShareFatal
		}
		foundCause := false
		for _, child := range joined.Unwrap() {
			if errors.Is(child, session.ErrTerminalDelivery) {
				continue
			}
			foundCause = true
			if ClassifySendError(child) == SendShareFatal {
				return SendShareFatal
			}
		}
		if foundCause {
			return SendPathEnded
		}
		return SendShareFatal
	}
	// A path-local sentinel anywhere in the chain is a definitive verdict and
	// must be recognized before the join aggregation below: sentinels wrap
	// subordinate diagnostics (ErrPeerViolation carries the decode detail via
	// %w), and aggregating first would let such an unknown child escalate one
	// receiver's outcome to share-fatal. The share-wide causes — drift and
	// terminal-delivery arbitration — have already claimed the chain above.
	if errors.Is(err, transportwebrtc.ErrTransport) ||
		errors.Is(err, transportwebrtc.ErrPeerProtocol) ||
		errors.Is(err, transportwebrtc.ErrTerminalNotAcknowledged) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, session.ErrSessionClosed) ||
		errors.Is(err, session.ErrPeerViolation) ||
		errors.Is(err, relay.ErrChannelClosed) ||
		errors.Is(err, relay.ErrConnClosed) ||
		errors.Is(err, transportwebrtc.ErrChannelClosed) ||
		errors.Is(err, transportwebrtc.ErrRemoteClosed) {
		return SendPathEnded
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return SendShareFatal
		}
		for _, child := range children {
			if ClassifySendError(child) == SendShareFatal {
				return SendShareFatal
			}
		}
		return SendPathEnded
	}
	if _, ok := errors.AsType[*session.Error](err); ok {
		return SendPathEnded
	}
	return SendShareFatal
}

// AcceptedReceiver is the transport-neutral unit accepted by share fan-out.
// ID is diagnostic only; Channel and Signaling are the owned domain inputs.
type AcceptedReceiver struct {
	ID        string
	Channel   session.FrameChannel
	Signaling Signaling
}

// ReceiverSessionSource is defined where share fan-out consumes it. Close owns
// source withdrawal: it must be idempotent, prevent future admissions, and unblock
// Accept. Run closes the source before canceling accepted session workers when its
// caller cancels, so remote peers observe source loss rather than a racing local bye.
type ReceiverSessionSource interface {
	Accept(context.Context) (AcceptedReceiver, error)
	Close() error
}

type ShareSenderOptions struct {
	ClassifyError   SendErrorClassifier
	OnReceiverStart func(AcceptedReceiver)
	OnReceiverEnd   func(AcceptedReceiver, error)
	OnPathError     func(Path, error)
}

// ShareSender owns share-level receiver fan-out, first share-fatal selection,
// sibling cancellation, and joining. It deliberately has no logging or exit-code
// policy so CLI, E2E, and the future engine consume the same lifecycle.
type ShareSender struct {
	source   ReceiverSessionSource
	sender   *Sender
	store    session.BlockStore
	sealer   session.Sealer
	classify SendErrorClassifier
	onStart  func(AcceptedReceiver)
	onEnd    func(AcceptedReceiver, error)
}

func NewShareSender(
	source ReceiverSessionSource,
	peers AnswerChannelFactory,
	store session.BlockStore,
	sealer session.Sealer,
	options ShareSenderOptions,
) (*ShareSender, error) {
	if source == nil || peers == nil || store == nil || sealer == nil {
		return nil, fmt.Errorf("%w: receiver source, peer factory, block store, and sealer are required", ErrNilDependency)
	}
	if options.ClassifyError == nil {
		options.ClassifyError = ClassifySendError
	}
	if options.OnReceiverStart == nil {
		options.OnReceiverStart = func(AcceptedReceiver) {}
	}
	if options.OnReceiverEnd == nil {
		options.OnReceiverEnd = func(AcceptedReceiver, error) {}
	}
	sender, err := NewSender(peers, SenderOptions{
		ClassifySessionError: options.ClassifyError,
		OnPathError:          options.OnPathError,
	})
	if err != nil {
		return nil, err
	}
	return &ShareSender{
		source:   source,
		sender:   sender,
		store:    store,
		sealer:   sealer,
		classify: options.ClassifyError,
		onStart:  options.OnReceiverStart,
		onEnd:    options.OnReceiverEnd,
	}, nil
}

type acceptResult struct {
	receiver AcceptedReceiver
	err      error
}

type receiverResult struct {
	receiver AcceptedReceiver
	err      error
}

// shareRun holds one Run invocation's fan-out state so the accept, serve, and
// shutdown phases decompose into methods that share it instead of threading a
// parameter list through each step.
type shareRun struct {
	share  *ShareSender
	ctx    context.Context
	runCtx context.Context
	cancel context.CancelFunc

	sourceCloseOnce sync.Once
	sourceCloseErr  error

	accepted chan acceptResult
	results  chan receiverResult
	workers  sync.WaitGroup

	acceptPending bool
	active        int
	accepting     bool
	ctxDone       <-chan struct{}
	fatal         error
	sourceErr     error
}

func (s *ShareSender) Run(ctx context.Context) (runErr error) {
	if ctx == nil {
		return fmt.Errorf("%w: context", ErrNilDependency)
	}
	// External cancellation is observed only by this coordinator. Accepted
	// sessions use a detached child so source withdrawal deterministically wins
	// before their deferred channel closes can emit per-session bye messages.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	run := &shareRun{
		share:     s,
		ctx:       ctx,
		runCtx:    runCtx,
		cancel:    cancel,
		accepted:  make(chan acceptResult, 1),
		results:   make(chan receiverResult),
		accepting: true,
		ctxDone:   ctx.Done(),
	}
	defer func() { runErr = run.joinSourceClose(runErr) }()
	if err := ctx.Err(); err != nil {
		run.closeSource()
		return err
	}
	run.startAccept()
	run.loop()
	run.workers.Wait()
	return run.result()
}

func (r *shareRun) closeSource() {
	r.sourceCloseOnce.Do(func() { r.sourceCloseErr = r.share.source.Close() })
}

// joinSourceClose folds the deferred source-withdrawal error into the run
// result. An external cancellation is superseded by it because the caller
// asked for shutdown and only the shutdown failure is news.
func (r *shareRun) joinSourceClose(runErr error) error {
	r.closeSource()
	if r.sourceCloseErr == nil {
		return runErr
	}
	shutdownErr := fmt.Errorf("connectivity: close receiver session source: %w", r.sourceCloseErr)
	if runErr == nil || errors.Is(runErr, context.Canceled) && r.ctx.Err() != nil {
		return shutdownErr
	}
	return errors.Join(runErr, shutdownErr)
}

func (r *shareRun) beginExternalShutdown() {
	if r.ctxDone == nil {
		return
	}
	r.ctxDone = nil
	r.accepting = false
	r.closeSource()
	r.cancel()
}

func (r *shareRun) startAccept() {
	r.acceptPending = true
	go func() {
		receiver, err := r.share.source.Accept(r.runCtx)
		r.accepted <- acceptResult{receiver: receiver, err: err}
	}()
}

func (r *shareRun) loop() {
	for r.acceptPending || r.active > 0 {
		if r.ctx.Err() != nil {
			r.beginExternalShutdown()
		}
		select {
		case result := <-r.accepted:
			r.handleAccepted(result)
		case result := <-r.results:
			r.handleServed(result)
		case <-r.ctxDone:
			r.beginExternalShutdown()
		}
	}
}

func (r *shareRun) handleAccepted(result acceptResult) {
	r.acceptPending = false
	if result.err != nil {
		if result.receiver.Channel != nil {
			_ = result.receiver.Channel.Close()
		}
		r.accepting = false
		if !errors.Is(result.err, io.EOF) && !errors.Is(result.err, context.Canceled) {
			r.sourceErr = fmt.Errorf("%w: %w", ErrReceiverSourceEnded, result.err)
		}
		r.cancel()
		return
	}
	if result.receiver.Channel == nil || result.receiver.Signaling == nil {
		r.fatal = fmt.Errorf("%w: accepted receiver channel and signaling are required", ErrNilDependency)
		r.accepting = false
		r.cancel()
		return
	}
	if r.ctx.Err() != nil {
		r.beginExternalShutdown()
	}
	if r.runCtx.Err() != nil {
		_ = result.receiver.Channel.Close()
		return
	}
	r.share.onStart(result.receiver)
	r.active++
	r.workers.Go(func() {
		err := r.share.sender.ServeReceiver(r.runCtx, result.receiver.Channel, result.receiver.Signaling, r.share.store, r.share.sealer)
		r.results <- receiverResult{receiver: result.receiver, err: err}
	})
	if r.accepting {
		r.startAccept()
	}
}

func (r *shareRun) handleServed(result receiverResult) {
	r.active--
	r.share.onEnd(result.receiver, result.err)
	if result.err != nil && !errors.Is(result.err, context.Canceled) && r.share.classify(result.err) == SendShareFatal && r.fatal == nil {
		r.fatal = result.err
		r.accepting = false
		r.cancel()
	}
}

// result reports the first share-fatal outcome ahead of source loss, which in
// turn outranks plain external cancellation.
func (r *shareRun) result() error {
	if r.fatal != nil {
		return r.fatal
	}
	if r.sourceErr != nil {
		return r.sourceErr
	}
	return r.ctx.Err()
}

type RelayReceiverSource struct{ conn *relay.SenderConn }

func NewRelayReceiverSource(conn *relay.SenderConn) (*RelayReceiverSource, error) {
	if conn == nil {
		return nil, fmt.Errorf("%w: relay sender connection", ErrNilDependency)
	}
	return &RelayReceiverSource{conn: conn}, nil
}

func (s *RelayReceiverSource) Accept(ctx context.Context) (AcceptedReceiver, error) {
	select {
	case <-ctx.Done():
		return AcceptedReceiver{}, ctx.Err()
	case channel, ok := <-s.conn.Sessions():
		if !ok {
			<-s.conn.Done()
			if err := s.conn.Err(); err != nil {
				return AcceptedReceiver{}, err
			}
			return AcceptedReceiver{}, io.EOF
		}
		signaling, err := NewRelaySignaling(channel)
		if err != nil {
			return AcceptedReceiver{}, err
		}
		return AcceptedReceiver{ID: channel.SessionID().String(), Channel: channel, Signaling: signaling}, nil
	}
}

func (s *RelayReceiverSource) Close() error { return s.conn.Close() }
