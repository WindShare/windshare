package nativeconnectivity

import (
	"errors"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/turncapacity"
	"github.com/windshare/windshare/connectivity/v2peer/peerset"
)

var ErrCapacityRequired = errors.New("application connectivity capacity is required")

// Capacity belongs to one Engine, above tasks and connection generations.
// Keeping the authorities together prevents factory replacement from minting
// admission tokens while each module retains its own scheduling policy.
type Capacity struct {
	admission *nativepeer.Admission
	turn      *turncapacity.Pool
	attempts  *peerset.Capacity
}

func NewCapacity(clock nativepeer.AdmissionClock) (*Capacity, error) {
	attempts, err := peerset.NewCapacity(peerset.DefaultAttemptCapacity)
	if err != nil {
		return nil, err
	}
	return &Capacity{
		admission: nativepeer.NewAdmission(clock),
		turn:      turncapacity.NewPool(),
		attempts:  attempts,
	}, nil
}

func (capacity *Capacity) NewNative(config nativepeer.Config) (*nativepeer.NativePeerConnectivity, error) {
	if capacity == nil {
		return nil, ErrCapacityRequired
	}
	config.Admission = capacity.admission
	config.TURNCapacity = capacity.turn
	return nativepeer.New(config)
}

func (capacity *Capacity) Attempts() *peerset.Capacity {
	return capacity.attempts
}
