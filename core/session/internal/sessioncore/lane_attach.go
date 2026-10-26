package sessioncore

import (
	"context"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	"io"
	"sync"
)

const (
	LaneGrantQueueLimit = 64
	laneNonceAttempts   = 4
)

// candidateChannelOwner makes the admission boundary consume one close
// authority. Failure paths and runtime lanes may converge on Close without
// calling provider cleanup more than once.
type Candidate struct {
	protocolsession.FrameChannel
	receive   func() <-chan framechannel.Frame
	closeOnce sync.Once
	closeErr  error
}

func NewCandidate(channel protocolsession.FrameChannel) *Candidate {
	if owner, ok := channel.(*Candidate); ok {
		return owner
	}
	// Transport receive subscription must follow lifecycle admission so runtime
	// cancellation owns every externally visible handshake side effect. OnceValue
	// retains the channel's single receive stream without subscribing eagerly.
	return &Candidate{
		FrameChannel: channel,
		receive:      sync.OnceValue(channel.Recv),
	}
}

func NewCandidateWithReceive(
	channel protocolsession.FrameChannel,
	receive <-chan framechannel.Frame,
) *Candidate {
	if owner, ok := channel.(*Candidate); ok {
		return owner
	}
	return &Candidate{
		FrameChannel: channel,
		receive:      func() <-chan framechannel.Frame { return receive },
	}
}

func (owner *Candidate) Recv() <-chan framechannel.Frame { return owner.receive() }

func (owner *Candidate) ConfirmAdmission(ctx context.Context) error {
	return ConfirmChannelAdmission(ctx, owner.FrameChannel)
}

func (owner *Candidate) Close() error {
	owner.closeOnce.Do(func() { owner.closeErr = owner.FrameChannel.Close() })
	return owner.closeErr
}

func ReadNonzeroLaneNonce(source io.Reader, size int) ([]byte, error) {
	for range laneNonceAttempts {
		value := make([]byte, size)
		if _, err := io.ReadFull(source, value); err != nil {
			return nil, err
		}
		if nonzeroBytes(value) {
			return value, nil
		}
	}
	return nil, session.ErrRuntimeConfig
}

func nonzeroBytes(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return true
		}
	}
	return false
}
