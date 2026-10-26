package sessioncore

import (
	"bytes"
	"context"
	"errors"
	cbor "github.com/fxamacker/cbor/v2"
	"github.com/windshare/windshare/core/framechannel"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	"io"
	"sync"
)

const (
	laneAttachBodyVersion = uint64(1)
	laneAttachRequestMode = uint64(0)
	laneAttachGrantMode   = uint64(1)
	LaneGrantQueueLimit   = 64
	laneNonceAttempts     = 4
)

var laneAttachDecMode = func() cbor.DecMode {
	mode, err := cbor.DecOptions{
		DupMapKey:       cbor.DupMapKeyEnforcedAPF,
		IndefLength:     cbor.IndefLengthForbidden,
		TagsMd:          cbor.TagsForbidden,
		MaxNestedLevels: 4,
		MaxMapPairs:     16,
	}.DecMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

type LaneAttachRequest struct{ requestedLaneID uint32 }

func EncodeLaneAttachRequest(requestedLaneID uint32) ([]byte, error) {
	return protocolsession.EncodeBody(map[uint64]any{
		0: laneAttachBodyVersion,
		1: laneAttachRequestMode,
		2: uint64(requestedLaneID),
	})
}

func DecodeLaneAttachRequest(encoded []byte) (LaneAttachRequest, error) {
	fields, err := decodeLaneAttachBody(encoded, 3)
	if err != nil || fields[0] != laneAttachBodyVersion || fields[1] != laneAttachRequestMode {
		return LaneAttachRequest{}, errors.Join(sessioncontract.ErrRuntimeConfig, err)
	}
	requested, ok := fields[2].(uint64)
	if !ok || requested > uint64(^uint32(0)) {
		return LaneAttachRequest{}, sessioncontract.ErrRuntimeConfig
	}
	return LaneAttachRequest{requestedLaneID: uint32(requested)}, nil
}

func EncodeLaneGrant(grant protocolsession.LaneGrant) ([]byte, error) {
	if grant.LaneID == 0 || grant.LaneEpoch == 0 || grant.OperationID.IsZero() || !nonzeroBytes(grant.AttachNonce[:]) {
		return nil, sessioncontract.ErrRuntimeConfig
	}
	return protocolsession.EncodeBody(map[uint64]any{
		0: laneAttachBodyVersion,
		1: laneAttachGrantMode,
		2: uint64(grant.LaneID),
		3: uint64(grant.LaneEpoch),
		4: grant.AttachNonce[:],
	})
}

func DecodeLaneGrant(encoded []byte, operationID protocolsession.OperationID) (sessioncontract.LaneAttachmentGrant, error) {
	fields, err := decodeLaneAttachBody(encoded, 5)
	if err != nil || fields[0] != laneAttachBodyVersion || fields[1] != laneAttachGrantMode || operationID.IsZero() {
		return sessioncontract.LaneAttachmentGrant{}, errors.Join(sessioncontract.ErrRuntimeConfig, err)
	}
	laneID, laneOK := fields[2].(uint64)
	epoch, epochOK := fields[3].(uint64)
	nonce, nonceOK := fields[4].([]byte)
	if !laneOK || !epochOK || !nonceOK || laneID == 0 || laneID > uint64(^uint32(0)) ||
		epoch == 0 || epoch > uint64(^uint32(0)) || len(nonce) != protocolsession.LaneAttachNonceBytes || !nonzeroBytes(nonce) {
		return sessioncontract.LaneAttachmentGrant{}, sessioncontract.ErrRuntimeConfig
	}
	grant := sessioncontract.LaneAttachmentGrant{
		LaneID: uint32(laneID), LaneEpoch: uint32(epoch), OperationID: operationID,
	}
	copy(grant.AttachNonce[:], nonce)
	return grant, nil
}

func decodeLaneAttachBody(encoded []byte, fieldCount int) (map[uint64]any, error) {
	var fields map[uint64]any
	if err := laneAttachDecMode.Unmarshal(encoded, &fields); err != nil || len(fields) != fieldCount {
		return nil, errors.Join(sessioncontract.ErrRuntimeConfig, err)
	}
	canonical, err := protocolsession.EncodeBody(fields)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return nil, errors.Join(sessioncontract.ErrRuntimeConfig, err)
	}
	return fields, nil
}

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
	return nil, sessioncontract.ErrRuntimeConfig
}

func nonzeroBytes(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return true
		}
	}
	return false
}
