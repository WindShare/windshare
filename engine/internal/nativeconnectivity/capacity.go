package nativeconnectivity

import (
	"errors"

	"github.com/windshare/windshare/connectivity/turnsession"

	"github.com/windshare/windshare/connectivity/nativepeer"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

var ErrCapacityRequired = errors.New("application connectivity capacity is required")

// Capacity belongs to one Engine, above tasks and connection generations.
// Keeping the authorities together prevents factory replacement from minting
// admission tokens while each module retains its own scheduling policy.
//
// Sender and receiver TURN permits are isolated because a receiver holds its
// permit until LANE_ATTACH; sharing one pool lets four outbound receiver lines
// deadlock or starve the paired local sender in the same Engine.
type Capacity struct {
	senderTURN   *turnsession.Capacity
	receiverTURN *turnsession.Capacity
	sends        *wsrtc.SendCapacity
	admission    *nativepeer.Admission
	monitor      nativepeer.NetworkMonitor
}

func NewCapacity(clock nativepeer.AdmissionClock, monitor nativepeer.NetworkMonitor) *Capacity {
	return &Capacity{
		senderTURN:   turnsession.NewCapacity(),
		receiverTURN: turnsession.NewCapacity(),
		sends:        wsrtc.NewSendCapacity(),
		admission:    nativepeer.NewAdmission(clock),
		monitor:      monitor,
	}
}

func (capacity *Capacity) NewNative(config nativepeer.Config) (*nativepeer.NativePeerConnectivity, error) {
	if capacity == nil {
		return nil, ErrCapacityRequired
	}
	config.Admission = capacity.admission
	if config.Monitor == nil {
		config.Monitor = capacity.monitor
	}
	return nativepeer.New(config)
}

func (capacity *Capacity) Sends() *wsrtc.SendCapacity {
	if capacity == nil {
		return nil
	}
	return capacity.sends
}

func (capacity *Capacity) SenderTURN() *turnsession.Capacity {
	if capacity == nil {
		return nil
	}
	return capacity.senderTURN
}

func (capacity *Capacity) ReceiverTURN() *turnsession.Capacity {
	if capacity == nil {
		return nil
	}
	return capacity.receiverTURN
}
