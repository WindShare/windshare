package relayv2

import (
	"errors"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

const (
	defaultReceiveFrames = 256
	defaultReceiveBytes  = 16 << 20
	maxReceiveSessions   = 256
)

var ErrReceiveCapacity = errors.New("relay v2 transport: invalid receive capacity")

func normalizeReceiveCapacity(capacity v2.ReceiveCapacity) (v2.ReceiveCapacity, error) {
	if capacity == (v2.ReceiveCapacity{}) {
		capacity = v2.ReceiveCapacity{Frames: defaultReceiveFrames, EncodedBytes: defaultReceiveBytes}
	}
	if err := capacity.Validate(); err != nil {
		return v2.ReceiveCapacity{}, errors.Join(ErrReceiveCapacity, err)
	}
	return capacity, nil
}
