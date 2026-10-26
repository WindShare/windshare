package v2peer

import (
	"errors"
	"time"

	pion "github.com/pion/webrtc/v4"

	"github.com/windshare/windshare/core/session/protocolsession"

	"github.com/windshare/windshare/core/session"
	receiversession "github.com/windshare/windshare/core/session/receiversession"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
)

// PeerRouteObserver supplies a local provider witness, never remote signaling.
type PeerRouteObserver interface {
	SelectedPeerRoute() (transferlanes.Route, bool)
}

func selectedPeerRoute(peer ReceiverPeerConnection) (transferlanes.Route, error) {
	if observer, ok := peer.(PeerRouteObserver); ok {
		route, valid := observer.SelectedPeerRoute()
		if valid && (route == transferlanes.RouteDirect || route == transferlanes.RouteTURN) {
			return route, nil
		}
		return 0, ErrNegotiation
	}
	provider, ok := peer.(interface{ SCTP() *pion.SCTPTransport })
	if !ok || provider.SCTP() == nil || provider.SCTP().Transport() == nil {
		return 0, ErrNegotiation
	}
	transport := provider.SCTP().Transport().ICETransport()
	if transport == nil {
		return 0, ErrNegotiation
	}
	pair, err := transport.GetSelectedCandidatePair()
	if err != nil || pair == nil || pair.Local == nil || pair.Remote == nil {
		return 0, errors.Join(ErrNegotiation, err)
	}
	if pair.Local.Typ == pion.ICECandidateTypeRelay || pair.Remote.Typ == pion.ICECandidateTypeRelay {
		return transferlanes.RouteTURN, nil
	}
	return transferlanes.RouteDirect, nil
}

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
	grant protocolsession.LaneGrant,
	admission receiversession.LaneAdmissionResult,
	err error,
) (receiverAdmissionSettlement, error) {
	identityMatches := admission.GrantOperationID == grant.OperationID &&
		admission.Lane == (session.LaneIdentity{ID: grant.LaneID, Epoch: grant.LaneEpoch}) &&
		!admission.GrantOperationID.IsZero()
	noRejection := admission.Rejection == (protocolsession.LaneRejection{})
	switch admission.Disposition {
	case receiversession.LaneAdmissionAccepted:
		if identityMatches && noRejection {
			switch {
			case admission.LaneInstallation == receiversession.LaneInstalled && err == nil:
				return receiverAdmissionInstalled, nil
			case admission.LaneInstallation == receiversession.LaneInstallationFailed && err != nil:
				return receiverAdmissionAcceptedInstallationFailed, err
			}
		}
	case receiversession.LaneAdmissionRejected:
		var rejected *session.LaneRejectedError
		if identityMatches && validReceiverLaneRejection(admission.Rejection) &&
			admission.LaneInstallation == receiversession.LaneInstallationNotAttempted &&
			errors.As(err, &rejected) && rejected.Rejection == admission.Rejection {
			return receiverAdmissionRejected, err
		}
	case receiversession.LaneAdmissionUnverified:
		if admission.GrantOperationID.IsZero() && admission.Lane == (session.LaneIdentity{}) &&
			noRejection &&
			admission.LaneInstallation == receiversession.LaneInstallationNotAttempted && err != nil {
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
