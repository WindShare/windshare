package v2peer

import (
	"errors"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

type receiverAdmissionSettlement uint8

const (
	receiverAdmissionUnverified receiverAdmissionSettlement = iota + 1
	receiverAdmissionRejected
	receiverAdmissionAcceptedInstallationFailed
	receiverAdmissionInstalled
)

func (settlement receiverAdmissionSettlement) authenticated() bool {
	return settlement == receiverAdmissionRejected ||
		settlement == receiverAdmissionAcceptedInstallationFailed ||
		settlement == receiverAdmissionInstalled
}

func receiverAttachmentSettlement(
	grant sessionruntime.LaneAttachmentGrant,
	admission sessionruntime.ReceiverLaneAdmissionResult,
	err error,
) (receiverAdmissionSettlement, error) {
	identityMatches := admission.GrantOperationID == grant.OperationID &&
		admission.Lane == (sessionruntime.LaneIdentity{ID: grant.LaneID, Epoch: grant.LaneEpoch}) &&
		!admission.GrantOperationID.IsZero()
	noRejection := admission.Rejection == (protocolsession.LaneRejection{})
	switch admission.Disposition {
	case sessionruntime.ReceiverLaneAdmissionAccepted:
		if identityMatches && noRejection {
			switch {
			case admission.LaneInstallation == sessionruntime.ReceiverLaneInstalled && err == nil:
				return receiverAdmissionInstalled, nil
			case admission.LaneInstallation == sessionruntime.ReceiverLaneInstallationFailed && err != nil:
				return receiverAdmissionAcceptedInstallationFailed, err
			}
		}
	case sessionruntime.ReceiverLaneAdmissionRejected:
		var rejected *sessionruntime.LaneRejectedError
		if identityMatches && validReceiverLaneRejection(admission.Rejection) &&
			admission.LaneInstallation == sessionruntime.ReceiverLaneInstallationNotAttempted &&
			errors.As(err, &rejected) && rejected.Rejection == admission.Rejection {
			return receiverAdmissionRejected, err
		}
	case sessionruntime.ReceiverLaneAdmissionUnverified:
		if admission.GrantOperationID.IsZero() && admission.Lane == (sessionruntime.LaneIdentity{}) &&
			noRejection &&
			admission.LaneInstallation == sessionruntime.ReceiverLaneInstallationNotAttempted && err != nil {
			return receiverAdmissionUnverified, err
		}
	}
	return receiverAdmissionUnverified, errors.Join(ErrProtocol, err)
}

func validReceiverLaneRejection(rejection protocolsession.LaneRejection) bool {
	switch rejection.Code {
	case protocolsession.LaneRejectUnknownSession,
		protocolsession.LaneRejectStaleEpoch,
		protocolsession.LaneRejectGrantExpired,
		protocolsession.LaneRejectGrantConsumed,
		protocolsession.LaneRejectAdmissionLimited,
		protocolsession.LaneRejectStopping,
		protocolsession.LaneRejectGrantMismatch:
	default:
		return false
	}
	return rejection.RetryAfter >= 0 &&
		rejection.RetryAfter <= protocolsession.MaxLaneRetryAfter &&
		rejection.RetryAfter%time.Millisecond == 0 &&
		(rejection.Code == protocolsession.LaneRejectAdmissionLimited || rejection.RetryAfter == 0)
}
