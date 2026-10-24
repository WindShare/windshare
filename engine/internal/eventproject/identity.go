package eventproject

import (
	"errors"

	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	appobs "github.com/windshare/windshare/engine/observation"
)

var ErrInvalidProjection = errors.New("workflow observation cannot be projected safely")

type ProjectionFailureReason uint8

const (
	ProjectionUnknownEnum ProjectionFailureReason = iota + 1
	ProjectionInvalidIdentity
	ProjectionInvalidStageFields
	ProjectionEventContract
)

type ProjectionError struct {
	reason    ProjectionFailureReason
	rejection appobs.ObservationRejection
}

func rejectedProjection(reason ProjectionFailureReason, field, rule string) error {
	return ProjectionError{reason: reason, rejection: appobs.ObservationRejection{Field: field, Rule: rule}}
}

func ProjectionRejection(err error) appobs.ObservationRejection {
	if diagnostic, ok := errors.AsType[appobs.OutputDiagnosticError](err); ok {
		return completeRejectionContext(diagnostic.Context)
	}
	if projection, ok := errors.AsType[ProjectionError](err); ok {
		return completeRejectionContext(projection.rejection)
	}
	return completeRejectionContext(appobs.ObservationRejection{})
}

func completeRejectionContext(context appobs.ObservationRejection) appobs.ObservationRejection {
	if context.Event == "" {
		context.Event = "observation"
	}
	if context.Source == "" {
		context.Source = "eventproject"
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
	return appobs.CaptureObservationRejection(context, context.Evidence()...)
}

func withRejectionContext(err error, context appobs.ObservationRejection) error {
	projection, ok := errors.AsType[ProjectionError](err)
	if !ok {
		projection = ProjectionError{reason: ProjectionEventContract}
	}
	if contract, ok := errors.AsType[appobs.EventContractError](err); ok {
		projection.rejection.Field, projection.rejection.Rule = contract.Field, contract.Rule
	}
	if projection.rejection.Field == "" {
		projection.rejection.Field, projection.rejection.Rule = "event", "projection_contract"
	}
	context.Field, context.Rule = projection.rejection.Field, projection.rejection.Rule
	fields := append(projection.rejection.Evidence(), context.Evidence()...)
	projection.rejection = completeRejectionContext(appobs.CaptureObservationRejection(context, fields...))
	return projection
}

func (err ProjectionError) Error() string                   { return ErrInvalidProjection.Error() }
func (err ProjectionError) Unwrap() error                   { return ErrInvalidProjection }
func (err ProjectionError) Reason() ProjectionFailureReason { return err.reason }

func invalidProjection(reason ProjectionFailureReason) error { return ProjectionError{reason: reason} }

func ObserverLossReason(err error) appobs.ObserverLossReason {
	if diagnostic, ok := errors.AsType[appobs.OutputDiagnosticError](err); ok {
		switch diagnostic.Reason {
		case appobs.ObserverLossUnknownEnum, appobs.ObserverLossInvalidIdentity, appobs.ObserverLossInvalidStageFields:
			return diagnostic.Reason
		}
	}
	if projection, ok := errors.AsType[ProjectionError](err); ok {
		switch projection.Reason() {
		case ProjectionUnknownEnum:
			return appobs.ObserverLossUnknownEnum
		case ProjectionInvalidIdentity:
			return appobs.ObserverLossInvalidIdentity
		case ProjectionInvalidStageFields:
			return appobs.ObserverLossInvalidStageFields
		}
	}
	return appobs.ObserverLossEventContract
}

func rejectedIdentityProjection(field, source string, raw []byte) error {
	return ProjectionError{
		reason: ProjectionInvalidIdentity,
		rejection: appobs.CaptureObservationRejection(appobs.ObservationRejection{
			Source: source, Field: field, Rule: "nonzero_16_bytes",
		}, appobs.RejectedIdentity(field, raw)),
	}
}

func RelaySessionID(raw []byte) (appobs.RelaySessionID, error) {
	result, err := appobs.NewRelaySessionID(raw)
	if err != nil {
		return appobs.RelaySessionID{}, rejectedIdentityProjection("relay_session_id", "eventproject.RelaySessionID", raw)
	}
	return result, nil
}

func ReceiveOperationID(value receivecontract.OperationID) (appobs.ReceiveOperationID, error) {
	if len(value.Bytes()) != appobs.IdentityBytes {
		return appobs.ReceiveOperationID{}, rejectedIdentityProjection("receive_operation_id", "eventproject.ReceiveOperationID", value.Bytes())
	}
	result, err := appobs.NewReceiveOperationID(value.Bytes())
	if err != nil {
		return appobs.ReceiveOperationID{}, rejectedIdentityProjection("receive_operation_id", "eventproject.ReceiveOperationID", value.Bytes())
	}
	return result, nil
}

func ProtocolSessionID(value protocolsession.ProtocolSessionID) (appobs.ProtocolSessionID, error) {
	if len(value.Bytes()) != appobs.IdentityBytes {
		return appobs.ProtocolSessionID{}, rejectedIdentityProjection("protocol_session_id", "eventproject.ProtocolSessionID", value.Bytes())
	}
	result, err := appobs.NewProtocolSessionID(value.Bytes())
	if err != nil {
		return appobs.ProtocolSessionID{}, rejectedIdentityProjection("protocol_session_id", "eventproject.ProtocolSessionID", value.Bytes())
	}
	return result, nil
}

func ProtocolOperationID(value protocolsession.OperationID) (appobs.ProtocolOperationID, error) {
	if len(value.Bytes()) != appobs.IdentityBytes {
		return appobs.ProtocolOperationID{}, rejectedIdentityProjection("protocol_operation_id", "eventproject.ProtocolOperationID", value.Bytes())
	}
	result, err := appobs.NewProtocolOperationID(value.Bytes())
	if err != nil {
		return appobs.ProtocolOperationID{}, rejectedIdentityProjection("protocol_operation_id", "eventproject.ProtocolOperationID", value.Bytes())
	}
	return result, nil
}

func TransferJobID(value transfer.TransferJobID) (appobs.TransferJobID, error) {
	if len(value.Bytes()) != appobs.IdentityBytes {
		return appobs.TransferJobID{}, rejectedIdentityProjection("transfer_job_id", "eventproject.TransferJobID", value.Bytes())
	}
	result, err := appobs.NewTransferJobID(value.Bytes())
	if err != nil {
		return appobs.TransferJobID{}, rejectedIdentityProjection("transfer_job_id", "eventproject.TransferJobID", value.Bytes())
	}
	return result, nil
}

func LaneIdentity(value sessionruntime.LaneIdentity) (appobs.LaneIdentity, error) {
	result, err := appobs.NewLaneIdentity(value.ID, value.Epoch)
	if err != nil {
		return appobs.LaneIdentity{}, ProjectionError{
			reason: ProjectionInvalidIdentity,
			rejection: appobs.CaptureObservationRejection(appobs.ObservationRejection{
				Source: "eventproject.LaneIdentity", Field: "lane", Rule: "nonzero_lane_id",
			}, appobs.RejectedUint("lane_id", uint64(value.ID)), appobs.RejectedUint("lane_epoch", uint64(value.Epoch))),
		}
	}
	return result, nil
}

func PeerPathID(value v2signal.PeerPathID) (appobs.PeerPathID, error) {
	result, err := appobs.NewPeerPathID(value[:])
	if err != nil {
		return appobs.PeerPathID{}, rejectedIdentityProjection("peer_path_id", "eventproject.PeerPathID", value[:])
	}
	return result, nil
}

func PeerAttemptID(value v2signal.AttemptID) (appobs.PeerAttemptID, error) {
	result, err := appobs.NewPeerAttemptID(value[:])
	if err != nil {
		return appobs.PeerAttemptID{}, rejectedIdentityProjection("peer_attempt_id", "eventproject.PeerAttemptID", value[:])
	}
	return result, nil
}
