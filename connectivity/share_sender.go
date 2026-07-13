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

func (s *ShareSender) Run(ctx context.Context) (runErr error) {
	if ctx == nil {
		return fmt.Errorf("%w: context", ErrNilDependency)
	}
	// External cancellation is observed only by this coordinator. Accepted
	// sessions use a detached child so source withdrawal deterministically wins
	// before their deferred channel closes can emit per-session bye messages.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	var sourceCloseOnce sync.Once
	var sourceCloseErr error
	closeSource := func() {
		sourceCloseOnce.Do(func() { sourceCloseErr = s.source.Close() })
	}
	defer func() {
		closeSource()
		if sourceCloseErr == nil {
			return
		}
		shutdownErr := fmt.Errorf("connectivity: close receiver session source: %w", sourceCloseErr)
		if runErr == nil || errors.Is(runErr, context.Canceled) && ctx.Err() != nil {
			runErr = shutdownErr
			return
		}
		runErr = errors.Join(runErr, shutdownErr)
	}()
	if err := ctx.Err(); err != nil {
		closeSource()
		return err
	}
	type acceptResult struct {
		receiver AcceptedReceiver
		err      error
	}
	type receiverResult struct {
		receiver AcceptedReceiver
		err      error
	}
	accepted := make(chan acceptResult, 1)
	results := make(chan receiverResult)
	var workers sync.WaitGroup
	acceptPending := false
	active := 0
	accepting := true
	ctxDone := ctx.Done()
	beginExternalShutdown := func() {
		if ctxDone == nil {
			return
		}
		ctxDone = nil
		accepting = false
		closeSource()
		cancel()
	}
	startAccept := func() {
		acceptPending = true
		go func() {
			receiver, err := s.source.Accept(runCtx)
			accepted <- acceptResult{receiver: receiver, err: err}
		}()
	}
	startAccept()
	var fatal error
	var sourceErr error

	for acceptPending || active > 0 {
		if ctx.Err() != nil {
			beginExternalShutdown()
		}
		select {
		case result := <-accepted:
			acceptPending = false
			if result.err != nil {
				if result.receiver.Channel != nil {
					_ = result.receiver.Channel.Close()
				}
				accepting = false
				if !errors.Is(result.err, io.EOF) && !errors.Is(result.err, context.Canceled) {
					sourceErr = fmt.Errorf("%w: %w", ErrReceiverSourceEnded, result.err)
				}
				cancel()
				continue
			}
			if result.receiver.Channel == nil || result.receiver.Signaling == nil {
				fatal = fmt.Errorf("%w: accepted receiver channel and signaling are required", ErrNilDependency)
				accepting = false
				cancel()
				continue
			}
			if ctx.Err() != nil {
				beginExternalShutdown()
			}
			if runCtx.Err() != nil {
				_ = result.receiver.Channel.Close()
				continue
			}
			s.onStart(result.receiver)
			active++
			workers.Go(func() {
				err := s.sender.ServeReceiver(runCtx, result.receiver.Channel, result.receiver.Signaling, s.store, s.sealer)
				results <- receiverResult{receiver: result.receiver, err: err}
			})
			if accepting {
				startAccept()
			}
		case result := <-results:
			active--
			s.onEnd(result.receiver, result.err)
			if result.err != nil && !errors.Is(result.err, context.Canceled) && s.classify(result.err) == SendShareFatal && fatal == nil {
				fatal = result.err
				accepting = false
				cancel()
			}
		case <-ctxDone:
			beginExternalShutdown()
		}
	}
	workers.Wait()
	if fatal != nil {
		return fatal
	}
	if sourceErr != nil {
		return sourceErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
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
