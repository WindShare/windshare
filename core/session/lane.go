package session

import (
	"fmt"
	"github.com/windshare/windshare/core/session/protocolsession"
)

// LaneRejectedError is safe to expose only after WS2N's sender signature has
// been verified against the exact LaneHello digest.
type LaneRejectedError struct {
	Rejection protocolsession.LaneRejection
}

func (err *LaneRejectedError) Error() string {
	return fmt.Sprintf("sender rejected lane attachment with code %d", err.Rejection.Code)
}

// LaneIdentity names one physical FrameChannel incarnation without leaking any
// provider or path metadata into core. Epoch zero is reserved for the transcript
// lane; authenticated attachments always use a positive epoch.
type LaneIdentity struct {
	ID    uint32
	Epoch uint32
}

func (identity LaneIdentity) Valid(initial bool) bool {
	return identity.ID != 0 && (initial || identity.Epoch != 0)
}
