package receiversession

import (
	"context"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/requestlane"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
)

type LaneAdmissionDisposition uint8

const (
	LaneAdmissionUnverified LaneAdmissionDisposition = iota + 1
	LaneAdmissionAccepted
	LaneAdmissionRejected
)

type LaneInstallation uint8

const (
	LaneInstallationNotAttempted LaneInstallation = iota + 1
	LaneInstalled
	LaneInstallationFailed
)

// LaneAdmissionResult separates authenticated sender settlement from
// local publication. A verified response remains authoritative even when the
// exact candidate owner cannot subsequently enter both receiver lane registries.
type LaneAdmissionResult struct {
	GrantOperationID protocolsession.OperationID
	Lane             session.LaneIdentity
	Disposition      LaneAdmissionDisposition
	Rejection        protocolsession.LaneRejection
	LaneInstallation LaneInstallation
}

func unverifiedReceiverLaneAdmission() LaneAdmissionResult {
	return LaneAdmissionResult{
		Disposition:      LaneAdmissionUnverified,
		LaneInstallation: LaneInstallationNotAttempted,
	}
}

func receiverLaneSettlement(grant protocolsession.LaneGrant) LaneAdmissionResult {
	return LaneAdmissionResult{
		GrantOperationID: grant.OperationID,
		Lane:             session.LaneIdentity{ID: grant.LaneID, Epoch: grant.LaneEpoch},
		LaneInstallation: LaneInstallationNotAttempted,
	}
}

// LaneDone identifies exact admitted ownership without consuming transport
// frames. Connectivity can recover one retired relay while other lanes remain.
func (runtime *Runtime) LaneDone(identity session.LaneIdentity) (<-chan struct{}, bool) {
	if runtime == nil || runtime.core == nil {
		return nil, false
	}
	return runtime.core.LaneDone(identity)
}

// AttachLane consumes one connectivity-owned candidate channel. Core closes
// that owner on every failure; successful installation transfers it to the
// runtime lane that will close it on exact detachment or session termination.
func (runtime *Runtime) AttachLane(
	ctx context.Context,
	grant protocolsession.LaneGrant,
	channel protocolsession.FrameChannel,
	route transferlanes.Route,
) (LaneAdmissionResult, error) {
	unverified := unverifiedReceiverLaneAdmission()
	if runtime == nil || channel == nil || ctx == nil || grant.LaneID == 0 ||
		grant.LaneEpoch == 0 || grant.OperationID.IsZero() {
		if channel != nil {
			_ = channel.Close()
		}
		return unverified, session.ErrRuntimeConfig
	}
	owner := sessioncore.NewCandidate(channel)
	transferred := false
	defer func() {
		if !transferred {
			_ = owner.Close()
		}
	}()
	admissionContext, endAdmission, err := runtime.core.BeginExternalAdmission(ctx)
	if err != nil {
		return unverified, err
	}
	defer endAdmission()
	hello, err := runtime.core.NewLaneHello(grant)
	if err != nil {
		return unverified, err
	}
	started := runtime.core.Now()
	if err := owner.Send(admissionContext, framechannel.Frame(hello.Encoded())); err != nil {
		return unverified, err
	}
	response, err := sessioncore.ReceiveHandshake(admissionContext, owner.Recv())
	if err != nil {
		return unverified, err
	}
	settlement := receiverLaneSettlement(grant)
	if len(response) == protocolsession.LaneRejectBytes {
		rejection, parseErr := protocolsession.ParseLaneReject(response, hello, runtime.sender)
		if parseErr != nil {
			return unverified, parseErr
		}
		settlement.Disposition = LaneAdmissionRejected
		settlement.Rejection = rejection
		return settlement, &session.LaneRejectedError{Rejection: rejection}
	}
	if _, err := protocolsession.ParseLaneAccept(response, hello, runtime.sender); err != nil {
		return unverified, err
	}
	responseTime := runtime.core.Now().Sub(started)
	settlement.Disposition = LaneAdmissionAccepted
	settlement.LaneInstallation = LaneInstallationFailed
	identity := settlement.Lane
	base := protocolsession.ControlBinding{
		ShareInstance: runtime.descriptor.ShareInstance(), ProtocolSessionID: runtime.ProtocolSessionID(),
		LaneID: identity.ID, LaneEpoch: identity.Epoch, Direction: protocolsession.DirectionSenderToReceiver,
	}
	authenticator, err := protocolsession.NewSenderControlAuthenticator(runtime.sender, base, runtime.semantic)
	if err != nil {
		return settlement, err
	}
	handOff := sessioncore.HandOff(owner, owner.Recv())
	blockLane := &receiverBlockLane{
		identity: identity, rpc: runtime.rpc, assembler: runtime.assembler,
		opener: runtime.opener, revisions: runtime.revisions,
	}
	err = runtime.core.InstallLane(identity, handOff, authenticator, requestlane.New(responseTime), func() error {
		return runtime.laneSet.Add(
			transferlanes.Identity{ID: identity.ID, Epoch: identity.Epoch},
			route,
			blockLane,
		)
	})
	if err != nil {
		return settlement, err
	}
	settlement.LaneInstallation = LaneInstalled
	transferred = true
	return settlement, nil
}
