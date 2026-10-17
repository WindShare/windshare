package protocolcontract

import (
	"fmt"
	"math"
	"slices"
)

const (
	relayDeliveryPrefixBytes  = 8
	relayDeliverySessionBytes = 8
	relayOpaqueHeaderBytes    = relayDeliveryPrefixBytes + relayDeliverySessionBytes + 4
	relayMinimumEnvelopeBytes = relayOpaqueHeaderBytes + 1
	relayMaximumEnvelopeBytes = relayOpaqueHeaderBytes + maxOpaqueCiphertextBytes
)

// Build from the layout, not a production codec: three implementations agreeing
// with one another must not hide an identical truncation or arithmetic mistake.
func relayDeliveryConsumedVectors(sessionID []byte) map[string]any {
	encode := func(frames, encodedBytes uint64) []byte {
		return slices.Concat([]byte("WS2B"), []byte{wireVersion, 0, 0, 0}, sessionID, u64(frames), u64(encodedBytes))
	}
	const aboveUint32 = uint64(math.MaxUint32) + 1
	const aboveSafeInteger = uint64(1<<53) + 1
	const maximumFrames = uint64(math.MaxUint64) / relayMinimumEnvelopeBytes
	const minimumFramesAtMaxBytes = uint64(math.MaxUint64)/relayMaximumEnvelopeBytes + 1
	var counts []any
	for _, test := range []struct {
		name                 string
		frames, encodedBytes uint64
		accepted             bool
	}{
		{"empty-prefix", 0, 0, true},
		{"minimum-envelope", 1, relayMinimumEnvelopeBytes, true},
		{"maximum-envelope", 1, relayMaximumEnvelopeBytes, true},
		{"mixed-envelope-sizes", 2, 100, true},
		{"above-runtime-windows", 257, 257 * relayMaximumEnvelopeBytes, true},
		{"above-u32", aboveUint32, aboveUint32 * relayMinimumEnvelopeBytes, true},
		{"above-javascript-safe-integer", aboveSafeInteger, aboveSafeInteger * relayMinimumEnvelopeBytes, true},
		{"maximum-bytes-minimum-frames", minimumFramesAtMaxBytes, math.MaxUint64, true},
		{"maximum-bytes-maximum-frames", maximumFrames, math.MaxUint64, true},
		{"frames-without-bytes", 1, 0, false},
		{"bytes-without-frames", 0, 1, false},
		{"below-minimum-envelope", 1, relayMinimumEnvelopeBytes - 1, false},
		{"above-maximum-envelope", 1, relayMaximumEnvelopeBytes + 1, false},
		{"batch-below-minimum", 2, 2*relayMinimumEnvelopeBytes - 1, false},
		{"batch-above-maximum", 2, 2*relayMaximumEnvelopeBytes + 1, false},
		{"maximum-bytes-too-few-frames", minimumFramesAtMaxBytes - 1, math.MaxUint64, false},
		{"maximum-bytes-too-many-frames", maximumFrames + 1, math.MaxUint64, false},
		{"both-u64-maxima", math.MaxUint64, math.MaxUint64, false},
		{"multiplication-wraparound", math.MaxUint64/relayMinimumEnvelopeBytes + 1, relayMinimumEnvelopeBytes - 1, false},
	} {
		counts = append(counts, map[string]any{
			"name": test.name, "totalFrames": fmt.Sprint(test.frames), "totalEncodedBytes": fmt.Sprint(test.encodedBytes),
			"encodedB64": b64(encode(test.frames, test.encodedBytes)), "accepted": test.accepted,
		})
	}

	valid := encode(2, 100)
	var malformed []any
	add := func(name string, encoded []byte) {
		malformed = append(malformed, map[string]any{"name": name, "encodedB64": b64(encoded)})
	}
	for end := range len(valid) {
		add(fmt.Sprintf("truncated-at-%d", end), valid[:end])
	}
	add("trailing-byte", slices.Concat(valid, []byte{0}))
	add("concatenated-controls", slices.Concat(valid, valid))
	for offset := range relayDeliveryPrefixBytes {
		mutated := slices.Clone(valid)
		mutated[offset] ^= 1
		add(fmt.Sprintf("invalid-prefix-byte-%d", offset), mutated)
	}
	zeroSession := slices.Clone(valid)
	clear(zeroSession[relayDeliveryPrefixBytes : relayDeliveryPrefixBytes+relayDeliverySessionBytes])
	add("zero-session-id", zeroSession)
	add("current-runtime-u32-credit", slices.Concat(
		[]byte("WS2B"), []byte{wireVersion, 0, 0, 0}, sessionID, u32(2), u32(100),
	))
	return map[string]any{
		"name": "relay-delivery-consumed", "relaySessionIdB64": b64(sessionID),
		"counts": counts, "malformed": malformed,
	}
}
