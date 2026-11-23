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
type Capacity struct {
	turn      *turnsession.Capacity
	sends     *wsrtc.SendCapacity
	admission *nativepeer.Admission
	monitor   nativepeer.NetworkMonitor
}

func NewCapacity(clock nativepeer.AdmissionClock, monitor nativepeer.NetworkMonitor) *Capacity {
	return &Capacity{
		turn:      turnsession.NewCapacity(),
		sends:     wsrtc.NewSendCapacity(),
		admission: nativepeer.NewAdmission(clock),
		monitor:   monitor,
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

func (c *Capacity) TURN() *turnsession.Capacity {
	if c == nil {
		return nil
	}
	return c.turn
}
