package webrtc

import (
	"fmt"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/session"
)

const (
	ChannelLabel    = "windshare-frame-channel"
	ChannelProtocol = "windshare-v1"

	defaultLowWaterBytes  uint64 = 256 * 1024
	defaultHighWaterBytes uint64 = 1024 * 1024
)

type flowControlProfile struct {
	lowWaterBytes  uint64
	highWaterBytes uint64
}

var defaultFlowControl = flowControlProfile{
	lowWaterBytes:  defaultLowWaterBytes,
	highWaterBytes: defaultHighWaterBytes,
}

// DefaultDataChannelInit returns a fresh in-band, ordered, reliable channel
// configuration. Pointer fields are intentionally recreated per call because
// Pion retains the caller's values while constructing the channel.
func DefaultDataChannelInit() *pion.DataChannelInit {
	ordered := true
	protocol := ChannelProtocol
	negotiated := false
	return &pion.DataChannelInit{
		Ordered:    &ordered,
		Protocol:   &protocol,
		Negotiated: &negotiated,
	}
}

func validateFlowControl(profile flowControlProfile) error {
	if profile.highWaterBytes == 0 || profile.lowWaterBytes >= profile.highWaterBytes {
		return fmt.Errorf(
			"%w: low=%d high=%d",
			ErrInvalidFlowControl,
			profile.lowWaterBytes,
			profile.highWaterBytes,
		)
	}
	return nil
}

func validateDataChannel(dc dataChannel) error {
	if err := validateDataChannelParameters(dc); err != nil {
		return err
	}
	switch dc.ReadyState() {
	case pion.DataChannelStateConnecting, pion.DataChannelStateOpen:
		return nil
	default:
		return fmt.Errorf("%w: initial state is %s", ErrInvalidDataChannel, dc.ReadyState())
	}
}

func validateDataChannelParameters(dc dataChannel) error {
	if dc.Label() != ChannelLabel {
		return fmt.Errorf("%w: label %q, want %q", ErrInvalidDataChannel, dc.Label(), ChannelLabel)
	}
	if dc.Protocol() != ChannelProtocol {
		return fmt.Errorf("%w: protocol %q, want %q", ErrInvalidDataChannel, dc.Protocol(), ChannelProtocol)
	}
	if !dc.Ordered() {
		return fmt.Errorf("%w: channel must be ordered", ErrInvalidDataChannel)
	}
	if dc.MaxPacketLifeTime() != nil || dc.MaxRetransmits() != nil {
		return fmt.Errorf("%w: retransmission limits make the channel unreliable", ErrInvalidDataChannel)
	}
	if dc.Negotiated() {
		return fmt.Errorf("%w: channel must use in-band negotiation", ErrInvalidDataChannel)
	}
	return nil
}

func validateMessageCapability(dc dataChannel) error {
	maximum := dc.maxMessageSize()
	if maximum < uint32(session.MaxFrameSize) {
		return fmt.Errorf(
			"%w: SCTP maximum message size is %d, need at least %d",
			ErrInvalidDataChannel,
			maximum,
			session.MaxFrameSize,
		)
	}
	return nil
}
