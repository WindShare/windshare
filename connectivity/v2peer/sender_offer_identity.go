package v2peer

import (
	"github.com/fxamacker/cbor/v2"
	"github.com/windshare/windshare/connectivity/v2signal"
)

var rejectedOfferIdentityDecoding = func() cbor.DecMode {
	mode, err := cbor.DecOptions{
		DupMapKey:        cbor.DupMapKeyEnforcedAPF,
		IndefLength:      cbor.IndefLengthForbidden,
		TagsMd:           cbor.TagsForbidden,
		MaxNestedLevels:  4,
		MaxArrayElements: 16,
		MaxMapPairs:      16,
	}.DecMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

// recoverOfferBinding is deliberately narrower than offer decoding. A malformed
// SDP or non-canonical tail still names the browser's evidence identity when the
// frozen version/path/attempt prefix is unambiguous, so that rejection needs its
// one terminal stream even though the offer itself must remain unusable.
func recoverOfferBinding(encoded []byte) (v2signal.Binding, bool) {
	var fields []cbor.RawMessage
	if err := rejectedOfferIdentityDecoding.Unmarshal(encoded, &fields); err != nil || len(fields) < 3 {
		return v2signal.Binding{}, false
	}
	var version uint64
	var pathBytes []byte
	var attemptBytes []byte
	if rejectedOfferIdentityDecoding.Unmarshal(fields[0], &version) != nil ||
		version != v2signal.SignalingSchemaVersion ||
		rejectedOfferIdentityDecoding.Unmarshal(fields[1], &pathBytes) != nil ||
		rejectedOfferIdentityDecoding.Unmarshal(fields[2], &attemptBytes) != nil ||
		len(pathBytes) != v2signal.IdentityBytes || len(attemptBytes) != v2signal.IdentityBytes {
		return v2signal.Binding{}, false
	}
	var binding v2signal.Binding
	copy(binding.PeerPathID[:], pathBytes)
	copy(binding.AttemptID[:], attemptBytes)
	if binding.Validate() != nil {
		return v2signal.Binding{}, false
	}
	return binding, true
}
