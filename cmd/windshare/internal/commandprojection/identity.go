package commandprojection

import (
	"errors"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

var ErrInvalidProjection = errors.New("command observation cannot be projected safely")

func ReceiveOperationID(value receivecontract.OperationID) (clievent.ReceiveOperationID, error) {
	if len(value.Bytes()) != clievent.IdentityBytes {
		return clievent.ReceiveOperationID{}, ErrInvalidProjection
	}
	result, err := clievent.NewReceiveOperationID(value.Bytes())
	if err != nil {
		return clievent.ReceiveOperationID{}, ErrInvalidProjection
	}
	return result, nil
}

func ProtocolSessionID(value protocolsession.ProtocolSessionID) (clievent.ProtocolSessionID, error) {
	if len(value.Bytes()) != clievent.IdentityBytes {
		return clievent.ProtocolSessionID{}, ErrInvalidProjection
	}
	result, err := clievent.NewProtocolSessionID(value.Bytes())
	if err != nil {
		return clievent.ProtocolSessionID{}, ErrInvalidProjection
	}
	return result, nil
}

func ProtocolOperationID(value protocolsession.OperationID) (clievent.ProtocolOperationID, error) {
	if len(value.Bytes()) != clievent.IdentityBytes {
		return clievent.ProtocolOperationID{}, ErrInvalidProjection
	}
	result, err := clievent.NewProtocolOperationID(value.Bytes())
	if err != nil {
		return clievent.ProtocolOperationID{}, ErrInvalidProjection
	}
	return result, nil
}

func TransferJobID(value transfer.TransferJobID) (clievent.TransferJobID, error) {
	if len(value.Bytes()) != clievent.IdentityBytes {
		return clievent.TransferJobID{}, ErrInvalidProjection
	}
	result, err := clievent.NewTransferJobID(value.Bytes())
	if err != nil {
		return clievent.TransferJobID{}, ErrInvalidProjection
	}
	return result, nil
}

func LaneIdentity(value sessionruntime.LaneIdentity) (clievent.LaneIdentity, error) {
	result, err := clievent.NewLaneIdentity(value.ID, value.Epoch)
	if err != nil {
		return clievent.LaneIdentity{}, ErrInvalidProjection
	}
	return result, nil
}

func PeerPathID(value v2signal.PeerPathID) (clievent.PeerPathID, error) {
	result, err := clievent.NewPeerPathID(value[:])
	if err != nil {
		return clievent.PeerPathID{}, ErrInvalidProjection
	}
	return result, nil
}

func PeerAttemptID(value v2signal.AttemptID) (clievent.PeerAttemptID, error) {
	result, err := clievent.NewPeerAttemptID(value[:])
	if err != nil {
		return clievent.PeerAttemptID{}, ErrInvalidProjection
	}
	return result, nil
}
