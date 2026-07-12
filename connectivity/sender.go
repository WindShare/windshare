package connectivity

import (
	"context"
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/session"
)

// Path identifies one independently backpressured data path for diagnostics.
type Path string

const (
	RelayPath Path = "relay"
	PeerPath  Path = "p2p"
)

type SenderOptions struct {
	// ClassifySessionError distinguishes source/sealer failures that make every
	// path unusable from transport and peer-local outcomes.
	ClassifySessionError SendErrorClassifier
	OnPathError          func(Path, error)
}

// Sender runs one independent orchestration instance per receiver. Relay and
// P2P SendSessions share only the immutable/concurrency-safe block source and
// sealer; their request loops and transport backpressure remain independent.
type Sender struct {
	peers         AnswerChannelFactory
	classifyError SendErrorClassifier
	onPathError   func(Path, error)
}

func NewSender(peers AnswerChannelFactory, options SenderOptions) (*Sender, error) {
	if peers == nil {
		return nil, fmt.Errorf("%w: answer channel factory", ErrNilDependency)
	}
	if options.ClassifySessionError == nil {
		options.ClassifySessionError = ClassifySendError
	}
	if options.OnPathError == nil {
		options.OnPathError = func(Path, error) {}
	}
	return &Sender{
		peers:         peers,
		classifyError: options.ClassifySessionError,
		onPathError:   options.OnPathError,
	}, nil
}

func (s *Sender) ServeReceiver(
	ctx context.Context,
	relayChannel session.FrameChannel,
	signaling Signaling,
	store session.BlockStore,
	sealer session.Sealer,
) error {
	if relayChannel == nil || signaling == nil || store == nil || sealer == nil {
		return fmt.Errorf("%w: relay channel, signaling, store, and sealer are required", ErrNilDependency)
	}
	if err := ctx.Err(); err != nil {
		_ = relayChannel.Close()
		return err
	}
	relaySession, err := session.NewSendSession(relayChannel, store, sealer)
	if err != nil {
		_ = relayChannel.Close()
		return err
	}

	receiverCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan senderPathResult, 2)
	go runSendPath(receiverCtx, RelayPath, relaySession, results)
	go func() {
		peerChannel, negotiateErr := s.peers.Answer(receiverCtx, signaling)
		if negotiateErr != nil {
			results <- senderPathResult{path: PeerPath, err: negotiateErr}
			return
		}
		if peerChannel == nil {
			results <- senderPathResult{
				path:                PeerPath,
				err:                 fmt.Errorf("%w: answer factory returned a nil channel", ErrNilDependency),
				constructionFailure: true,
			}
			return
		}
		peerSession, sessionErr := session.NewSendSession(peerChannel, store, sealer)
		if sessionErr != nil {
			_ = peerChannel.Close()
			results <- senderPathResult{path: PeerPath, err: sessionErr, constructionFailure: true}
			return
		}
		runSendPath(receiverCtx, PeerPath, peerSession, results)
	}()

	var fatal error
	for range 2 {
		result := <-results
		if result.err == nil || errors.Is(result.err, context.Canceled) && receiverCtx.Err() != nil {
			continue
		}
		if result.constructionFailure || result.sessionFailure && s.classifyError(result.err) == SendShareFatal {
			if fatal == nil {
				fatal = result.err
				cancel()
			}
			continue
		}
		if !errors.Is(result.err, ErrSignalingClosed) {
			s.onPathError(result.path, result.err)
		}
	}
	if fatal != nil {
		return fatal
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

type senderPathResult struct {
	path                Path
	err                 error
	sessionFailure      bool
	constructionFailure bool
}

func runSendPath(ctx context.Context, path Path, sendSession *session.SendSession, results chan<- senderPathResult) {
	results <- senderPathResult{
		path:           path,
		err:            sendSession.Run(ctx),
		sessionFailure: true,
	}
}
