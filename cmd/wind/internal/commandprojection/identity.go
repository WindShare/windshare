package commandprojection

import (
	"errors"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

var ErrInvalidProjection = errors.New("command observation cannot be projected safely")

type ProjectionFailureReason uint8

const (
	ProjectionUnknownEnum ProjectionFailureReason = iota + 1
	ProjectionInvalidIdentity
	ProjectionInvalidStageFields
	ProjectionEventContract
)

type ProjectionError struct {
	reason    ProjectionFailureReason
	rejection clievent.ObservationRejection
}

func rejectedProjection(reason ProjectionFailureReason, field, rule string) error {
	return ProjectionError{reason: reason, rejection: clievent.ObservationRejection{Field: field, Rule: rule}}
}

func ProjectionRejection(err error) clievent.ObservationRejection {
	if projection, ok := errors.AsType[ProjectionError](err); ok && projection.rejection.Valid() {
		return projection.rejection
	}
	return clievent.ObservationRejection{Stage: "unknown", Field: "event", Rule: "projection_contract"}
}

func withRejectionContext(err error, context clievent.ObservationRejection) error {
	projection, ok := errors.AsType[ProjectionError](err)
	if !ok {
		projection = ProjectionError{reason: ProjectionEventContract}
	}
	if contract, ok := errors.AsType[clievent.EventContractError](err); ok {
		projection.rejection.Field, projection.rejection.Rule = contract.Field, contract.Rule
	}
	if projection.rejection.Field == "" {
		projection.rejection.Field, projection.rejection.Rule = "event", "projection_contract"
	}
	projection.rejection.Stage = context.Stage
	projection.rejection.Session = context.Session
	projection.rejection.Operation = context.Operation
	projection.rejection.Revision = context.Revision
	return projection
}

func (err ProjectionError) Error() string                   { return ErrInvalidProjection.Error() }
func (err ProjectionError) Unwrap() error                   { return ErrInvalidProjection }
func (err ProjectionError) Reason() ProjectionFailureReason { return err.reason }

func invalidProjection(reason ProjectionFailureReason) error { return ProjectionError{reason: reason} }

func ObserverLossReason(err error) clievent.ObserverLossReason {
	if projection, ok := errors.AsType[ProjectionError](err); ok {
		switch projection.Reason() {
		case ProjectionUnknownEnum:
			return clievent.ObserverLossUnknownEnum
		case ProjectionInvalidIdentity:
			return clievent.ObserverLossInvalidIdentity
		case ProjectionInvalidStageFields:
			return clievent.ObserverLossInvalidStageFields
		}
	}
	return clievent.ObserverLossEventContract
}

func RelaySessionID(raw []byte) (clievent.RelaySessionID, error) {
	result, err := clievent.NewRelaySessionID(raw)
	if err != nil {
		return clievent.RelaySessionID{}, invalidProjection(ProjectionInvalidIdentity)
	}
	return result, nil
}

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
