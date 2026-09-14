package sessionruntime

import (
	"context"
	"time"

	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/requestlane"
	"github.com/windshare/windshare/core/transfer"
)

type ReceiverLaneAdmissionDisposition uint8

const (
	ReceiverLaneAdmissionUnverified ReceiverLaneAdmissionDisposition = iota + 1
	ReceiverLaneAdmissionAccepted
	ReceiverLaneAdmissionRejected
)

type ReceiverLaneInstallation uint8

const (
	ReceiverLaneInstallationNotAttempted ReceiverLaneInstallation = iota + 1
	ReceiverLaneInstalled
	ReceiverLaneInstallationFailed
)

// ReceiverLaneAdmissionResult separates authenticated sender settlement from
// local publication. A verified response remains authoritative even when the
// exact candidate owner cannot subsequently enter both receiver lane registries.
type ReceiverLaneAdmissionResult struct {
	GrantOperationID protocolsession.OperationID
	Lane             LaneIdentity
	Disposition      ReceiverLaneAdmissionDisposition
	Rejection        protocolsession.LaneRejection
	LaneInstallation ReceiverLaneInstallation
}

func unverifiedReceiverLaneAdmission() ReceiverLaneAdmissionResult {
	return ReceiverLaneAdmissionResult{
		Disposition:      ReceiverLaneAdmissionUnverified,
		LaneInstallation: ReceiverLaneInstallationNotAttempted,
	}
}

func receiverLaneSettlement(grant LaneAttachmentGrant) ReceiverLaneAdmissionResult {
	return ReceiverLaneAdmissionResult{
		GrantOperationID: grant.OperationID,
		Lane:             LaneIdentity{ID: grant.LaneID, Epoch: grant.LaneEpoch},
		LaneInstallation: ReceiverLaneInstallationNotAttempted,
	}
}

// LaneDone identifies exact admitted ownership without consuming transport
// frames. Connectivity can recover one retired relay while other lanes remain.
func (runtime *ReceiverRuntime) LaneDone(identity LaneIdentity) (<-chan struct{}, bool) {
	if runtime == nil || runtime.runtimeCore == nil {
		return nil, false
	}
	runtime.lanes.mu.Lock()
	defer runtime.lanes.mu.Unlock()
	lane := runtime.lanes.active[identity.ID]
	if lane == nil || lane.identity != identity {
		return nil, false
	}
	return lane.done, true
}

// AttachLane consumes one connectivity-owned candidate channel. Core closes
// that owner on every failure; successful installation transfers it to the
// runtime lane that will close it on exact detachment or session termination.
func (runtime *ReceiverRuntime) AttachLane(
	ctx context.Context,
	grant LaneAttachmentGrant,
	channel protocolsession.FrameChannel,
	route transfer.LaneRoute,
) (ReceiverLaneAdmissionResult, error) {
	unverified := unverifiedReceiverLaneAdmission()
	if runtime == nil || channel == nil || ctx == nil || grant.LaneID == 0 ||
		grant.LaneEpoch == 0 || grant.OperationID.IsZero() {
		if channel != nil {
			_ = channel.Close()
		}
		return unverified, ErrRuntimeConfig
	}
	owner := newCandidateChannelOwner(channel)
	transferred := false
	defer func() {
		if !transferred {
			_ = owner.Close()
		}
	}()
	admissionContext, endAdmission, err := runtime.beginExternalAdmission(ctx)
	if err != nil {
		return unverified, err
	}
	defer endAdmission()
	hello, err := protocolsession.NewLaneHello(
		runtime.descriptor.ShareInstance(), runtime.ProtocolSessionID(), grant.LaneID, grant.LaneEpoch,
		grant.OperationID, grant.AttachNonce[:], runtime.keys.ReceiverToSender(),
	)
	if err != nil {
		return unverified, err
	}
	started := runtime.now()
	if err := owner.Send(admissionContext, framechannel.Frame(hello.Encoded())); err != nil {
		return unverified, err
	}
	response, err := receiveHandshake(admissionContext, owner.Recv())
	if err != nil {
		return unverified, err
	}
	settlement := receiverLaneSettlement(grant)
	if len(response) == protocolsession.LaneRejectBytes {
		rejection, parseErr := protocolsession.ParseLaneReject(response, hello, runtime.publicKey)
		if parseErr != nil {
			return unverified, parseErr
		}
		settlement.Disposition = ReceiverLaneAdmissionRejected
		settlement.Rejection = rejection
		return settlement, &LaneRejectedError{Rejection: rejection}
	}
	if _, err := protocolsession.ParseLaneAccept(response, hello, runtime.publicKey); err != nil {
		return unverified, err
	}
	responseTime := runtime.now().Sub(started)
	settlement.Disposition = ReceiverLaneAdmissionAccepted
	settlement.LaneInstallation = ReceiverLaneInstallationFailed
	identity := settlement.Lane
	base := protocolsession.ControlBinding{
		ShareInstance: runtime.descriptor.ShareInstance(), ProtocolSessionID: runtime.ProtocolSessionID(),
		LaneID: identity.ID, LaneEpoch: identity.Epoch, Direction: protocolsession.DirectionSenderToReceiver,
	}
	authenticator, err := protocolsession.NewSenderControlAuthenticator(runtime.publicKey, base, runtime.semantic)
	if err != nil {
		return settlement, err
	}
	handOff := &handedOffChannel{FrameChannel: owner, receive: owner.Recv()}
	blockLane := &receiverBlockLane{
		identity: identity, rpc: runtime.rpc, assembler: runtime.assembler,
		opener: runtime.opener, revisions: runtime.revisions,
	}
	_, err = runtime.lanes.addWithAdmission(identity, handOff, authenticator, false, func(lane *runtimeLane) error {
		lane.requests = requestlane.New(responseTime)
		return runtime.laneSet.Add(
			transfer.LaneIdentity{ID: identity.ID, Epoch: identity.Epoch},
			route,
			blockLane,
		)
	})
	if err != nil {
		return settlement, err
	}
	settlement.LaneInstallation = ReceiverLaneInstalled
	transferred = true
	return settlement, nil
}

// Selection and reservation share the registry lock, so concurrent callers see
// each other's pending work and no request can reserve an already detached lane.
func (lanes *runtimeLanes) selectRequestLane(
	preferred *LaneIdentity, kind protocolsession.MessageKind,
) (selectedLane, *requestlane.Reservation, requestlane.Estimate, error) {
	if preferred != nil || !requestlane.Managed(kind) {
		selected, err := lanes.selectLane(preferred)
		return selected, nil, requestlane.Estimate{}, err
	}
	lanes.mu.Lock()
	defer lanes.mu.Unlock()
	if lanes.stopping || lanes.runtime.ctx.Err() != nil {
		return selectedLane{}, nil, requestlane.Estimate{}, ErrRuntimeClosed
	}
	now := lanes.runtime.now()
	var best *runtimeLane
	var estimate requestlane.Estimate
	for _, id := range lanes.order {
		lane := lanes.active[id]
		if !lane.usableLocked() {
			continue
		}
		var queued time.Duration
		if lanes.queuedContent != nil {
			queued = lanes.queuedContent(lane.identity)
		}
		candidate := lane.requests.Estimate(kind, now, queued)
		if best == nil || candidate.Expected < estimate.Expected {
			best, estimate = lane, candidate
		}
	}
	if best == nil {
		return selectedLane{}, nil, requestlane.Estimate{}, ErrLaneUnavailable
	}
	return best.selected(), best.requests.Reserve(kind, now), estimate, nil
}
