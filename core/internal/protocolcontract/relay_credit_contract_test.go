package protocolcontract

import "slices"

const (
	relayCreditMaximumFrames = 64
	relayCreditMaximumBytes  = 4 << 20
)

func relaySessionCreditVectors(sessionID []byte) map[string]any {
	var grants []any
	for _, grant := range []struct {
		name          string
		frames, bytes uint32
	}{
		{"frame-only", relayCreditMaximumFrames, 0},
		{"byte-only", 0, relayCreditMaximumBytes},
		{"independent-deltas", 1, 1},
		{"maximum-byte-delta", 1, relayCreditMaximumBytes},
		{"full-window", relayCreditMaximumFrames, relayCreditMaximumBytes},
	} {
		encoded := slices.Concat([]byte("WS2W"), []byte{wireVersion, 0, 0, 0}, sessionID, u32(grant.frames), u32(grant.bytes))
		grants = append(grants, map[string]any{
			"name": grant.name, "frames": grant.frames, "bytes": grant.bytes, "encodedB64": b64(encoded),
		})
	}
	return map[string]any{
		"name": "bidirectional-relay-session-credit", "relaySessionIdB64": b64(sessionID), "grants": grants,
	}
}

func relaySessionCreditSemantics() map[string]any {
	return map[string]any{
		"directions":             []string{"sender-to-receiver", "receiver-to-sender"},
		"maximumAvailableFrames": relayCreditMaximumFrames,
		"maximumAvailableBytes":  relayCreditMaximumBytes,
		"deltaFieldsIndependent": true,
		"nonzeroDeltaRequired":   true,
		"senderInitialFrames":    relayCreditMaximumFrames,
		"senderInitialBytes":     relayCreditMaximumBytes,
		"receiverInitialFrames":  0,
		"receiverInitialBytes":   0,
		"receiverAllocation":     "fair-shared-destination-capacity",
		"opaqueFrameCost":        []string{"one-frame", "full-encoded-route-bytes"},
		"controlConsumesCredit":  false,
	}
}
