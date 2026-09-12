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
	if projection, ok := errors.AsType[ProjectionError](err); ok {
		return completeRejectionContext(projection.rejection)
	}
	return completeRejectionContext(clievent.ObservationRejection{})
}

func completeRejectionContext(context clievent.ObservationRejection) clievent.ObservationRejection {
	if context.Event == "" {
		context.Event = "observation"
	}
	if context.Source == "" {
		context.Source = "commandprojection"
	}
	if context.Stage == "" {
		context.Stage = "unknown"
	}
	if context.Field == "" {
		context.Field = "event"
	}
	if context.Rule == "" {
		context.Rule = "projection_contract"
	}
	return clievent.CaptureObservationRejection(context, context.Evidence()...)
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
	context.Field, context.Rule = projection.rejection.Field, projection.rejection.Rule
	fields := append(projection.rejection.Evidence(), context.Evidence()...)
	projection.rejection = completeRejectionContext(clievent.CaptureObservationRejection(context, fields...))
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

func rejectedIdentityProjection(field, source string, raw []byte) error {
	return ProjectionError{
		reason: ProjectionInvalidIdentity,
		rejection: clievent.CaptureObservationRejection(clievent.ObservationRejection{
			Source: source, Field: field, Rule: "nonzero_16_bytes",
		}, clievent.RejectedIdentity(field, raw)),
	}
}

func RelaySessionID(raw []byte) (clievent.RelaySessionID, error) {
	result, err := clievent.NewRelaySessionID(raw)
	if err != nil {
		return clievent.RelaySessionID{}, rejectedIdentityProjection("relay_session_id", "commandprojection.RelaySessionID", raw)
	}
	return result, nil
}

func ReceiveOperationID(value receivecontract.OperationID) (clievent.ReceiveOperationID, error) {
	if len(value.Bytes()) != clievent.IdentityBytes {
		return clievent.ReceiveOperationID{}, rejectedIdentityProjection("receive_operation_id", "commandprojection.ReceiveOperationID", value.Bytes())
	}
	result, err := clievent.NewReceiveOperationID(value.Bytes())
	if err != nil {
		return clievent.ReceiveOperationID{}, rejectedIdentityProjection("receive_operation_id", "commandprojection.ReceiveOperationID", value.Bytes())
	}
	return result, nil
}

func ProtocolSessionID(value protocolsession.ProtocolSessionID) (clievent.ProtocolSessionID, error) {
	if len(value.Bytes()) != clievent.IdentityBytes {
		return clievent.ProtocolSessionID{}, rejectedIdentityProjection("protocol_session_id", "commandprojection.ProtocolSessionID", value.Bytes())
	}
	result, err := clievent.NewProtocolSessionID(value.Bytes())
	if err != nil {
		return clievent.ProtocolSessionID{}, rejectedIdentityProjection("protocol_session_id", "commandprojection.ProtocolSessionID", value.Bytes())
	}
	return result, nil
}

func ProtocolOperationID(value protocolsession.OperationID) (clievent.ProtocolOperationID, error) {
	if len(value.Bytes()) != clievent.IdentityBytes {
		return clievent.ProtocolOperationID{}, rejectedIdentityProjection("protocol_operation_id", "commandprojection.ProtocolOperationID", value.Bytes())
	}
	result, err := clievent.NewProtocolOperationID(value.Bytes())
	if err != nil {
		return clievent.ProtocolOperationID{}, rejectedIdentityProjection("protocol_operation_id", "commandprojection.ProtocolOperationID", value.Bytes())
	}
	return result, nil
}

func TransferJobID(value transfer.TransferJobID) (clievent.TransferJobID, error) {
	if len(value.Bytes()) != clievent.IdentityBytes {
		return clievent.TransferJobID{}, rejectedIdentityProjection("transfer_job_id", "commandprojection.TransferJobID", value.Bytes())
	}
	result, err := clievent.NewTransferJobID(value.Bytes())
	if err != nil {
		return clievent.TransferJobID{}, rejectedIdentityProjection("transfer_job_id", "commandprojection.TransferJobID", value.Bytes())
	}
	return result, nil
}

func LaneIdentity(value sessionruntime.LaneIdentity) (clievent.LaneIdentity, error) {
	result, err := clievent.NewLaneIdentity(value.ID, value.Epoch)
	if err != nil {
		return clievent.LaneIdentity{}, ProjectionError{
			reason: ProjectionInvalidIdentity,
			rejection: clievent.CaptureObservationRejection(clievent.ObservationRejection{
				Source: "commandprojection.LaneIdentity", Field: "lane", Rule: "nonzero_lane_id",
			}, clievent.RejectedUint("lane_id", uint64(value.ID)), clievent.RejectedUint("lane_epoch", uint64(value.Epoch))),
		}
	}
	return result, nil
}

func PeerPathID(value v2signal.PeerPathID) (clievent.PeerPathID, error) {
	result, err := clievent.NewPeerPathID(value[:])
	if err != nil {
		return clievent.PeerPathID{}, rejectedIdentityProjection("peer_path_id", "commandprojection.PeerPathID", value[:])
	}
	return result, nil
}

func PeerAttemptID(value v2signal.AttemptID) (clievent.PeerAttemptID, error) {
	result, err := clievent.NewPeerAttemptID(value[:])
	if err != nil {
		return clievent.PeerAttemptID{}, rejectedIdentityProjection("peer_attempt_id", "commandprojection.PeerAttemptID", value[:])
	}
	return result, nil
}
