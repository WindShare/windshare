package protocolcontract

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"maps"
	"slices"
)

// These fixtures build wire bytes independently of all three production codecs.
const relayCapacityFrames uint64 = 16
const relayCapacityBytes uint64 = relayCapacityFrames * (20 + 65536)
const relayResumeCapacityFrames uint64 = 8
const relayResumeCapacityBytes uint64 = relayResumeCapacityFrames * (20 + 65536)

func relayCapacityFields() []byte {
	return slices.Concat(u64(relayCapacityFrames), u64(relayCapacityBytes))
}

func vectorBytes(value any) []byte {
	raw, err := base64.StdEncoding.DecodeString(value.(string))
	if err != nil {
		panic(err)
	}
	return raw
}

func capacityRegistrationFields(f *fixture, vector map[string]any, prefix string) {
	capacity := relayCapacityFields()
	initKey, preimageKey, proofKey, signatureKey := "registerInitB64", "preimageB64", "registerProofB64", "signatureB64"
	if prefix == "resume" {
		capacity = slices.Concat(u64(relayResumeCapacityFrames), u64(relayResumeCapacityBytes))
		initKey, preimageKey, proofKey, signatureKey = "resumeInitB64", "resumePreimageB64", "resumeProofB64", "resumeSignatureB64"
	}
	vector[initKey] = b64(slices.Concat(vectorBytes(vector[initKey]), capacity))
	preimage := slices.Concat(vectorBytes(vector[preimageKey]), capacity)
	signature := ed25519.Sign(f.edPrivate, preimage)
	proof := vectorBytes(vector[proofKey])
	vector[preimageKey], vector[signatureKey] = b64(preimage), b64(signature)
	vector[proofKey] = b64(slices.Concat(proof[:relayHeaderBytes+relaySenderKeyBytes], signature))
}

func relayCapacityRegistrationVector(f *fixture, original map[string]any) map[string]any {
	result := maps.Clone(original)
	result["name"] = "relay-capacity-registration"
	capacityRegistrationFields(f, result, "register")
	capacityRegistrationFields(f, result, "resume")
	result["joinB64"] = b64(slices.Concat(vectorBytes(result["joinB64"]), relayCapacityFields()))
	result["receiveFrames"], result["receiveBytes"] = fmt.Sprint(relayCapacityFrames), fmt.Sprint(relayCapacityBytes)
	result["resumeReceiveFrames"], result["resumeReceiveBytes"] = fmt.Sprint(relayResumeCapacityFrames), fmt.Sprint(relayResumeCapacityBytes)
	delete(result, "receiveCreditB64")
	return result
}

func relayCapacityDescriptorVectors(f *fixture, original map[string]any) map[string]any {
	result := maps.Clone(original)
	result["name"] = "relay-capacity-descriptor-upload"
	var cases []any
	for _, item := range original["cases"].([]any) {
		entry := maps.Clone(item.(map[string]any))
		capacityRegistrationFields(f, entry, "register")
		cases = append(cases, entry)
	}
	result["cases"] = cases
	return result
}

func relayCapacityDecodingVectors(r relayControlFixture) map[string]any {
	var cases []relayDecodingCase
	for _, role := range []string{"register", "resume", "join"} {
		var base []byte
		reserved := []int{6, 7}
		switch role {
		case "register":
			base = r.registration(relayModeFresh).encode()
		case "resume":
			base = r.registration(relayModeResume).encode()
		case "join":
			base = slices.Concat([]byte("WS2J"), []byte{wireVersion, 0, 0, 0}, r.f.shareIDRaw)
			reserved = []int{5, 6, 7}
		}
		canonical := slices.Concat(base, relayCapacityFields())
		variants := []relayFrameVariant{
			rejectedFrame("missing-capacity", base),
			rejectedFrame("zero-frames", withZeroField(canonical, len(base), 8)),
			rejectedFrame("zero-bytes", withZeroField(canonical, len(base)+8, 8)),
			rejectedFrame("zero-share-id", withZeroField(canonical, relayHeaderBytes, relayShareIDBytes)),
		}
		for length := 0; length < len(canonical); length++ {
			variants = append(variants, rejectedFrame(fmt.Sprintf("length-%d", length), canonical[:length]))
		}
		for _, capacity := range [][2]uint64{{1, 1}, {1, ^uint64(0)}, {^uint64(0), 1}, {1<<53 + 1, 1<<53 + 3}, {^uint64(0), ^uint64(0)}} {
			variants = append(variants, relayFrameVariant{
				name:    fmt.Sprintf("capacity-%d-%d", capacity[0], capacity[1]),
				encoded: slices.Concat(base, u64(capacity[0]), u64(capacity[1])), accepted: true,
			})
		}
		if role != "join" {
			variants = append(variants,
				rejectedFrame("unknown-mode", withByte(canonical, 5, 2)),
				rejectedFrame("zero-share-instance", withZeroField(canonical, relayHeaderBytes+relayShareIDBytes, relayShareInstanceBytes)),
				rejectedFrame("pk-hash-not-deriving-share-id", withFlippedByte(canonical, relayHeaderBytes+relayShareIDBytes+relayShareInstanceBytes)),
			)
		}
		cases = append(cases, relayFrameCases(relayFrameSample{
			label: role, direction: relayClientToRelay, canonical: canonical, reserved: reserved, variants: variants,
		})...)
	}
	return map[string]any{"name": "relay-capacity-frame-decoding", "cases": cases}
}

func relayCapacityProofVectors(r relayControlFixture) map[string]any {
	r.receiveCapacity = relayCapacityFields()
	result := relayControlProofVectors(r)
	result["name"] = "relay-capacity-proof-authentication"
	cases := result["cases"].([]relayProofCase)
	for _, mode := range []byte{relayModeFresh, relayModeResume} {
		init, challenge := r.registration(mode), r.challenge(mode)
		domain := domainRegister
		if mode == relayModeResume {
			domain = domainResume
		}
		signature := ed25519.Sign(r.f.edPrivate, init.preimage(domain, r.relayIdentity, challenge))
		proof := relaySenderProof("WS2P", mode, r.f.edPublic, signature)
		for _, offset := range []int{0, 8} {
			changedInit := init
			changedInit.receiveCapacity = withFlippedByte(init.receiveCapacity, offset+7)
			cases = append(cases, newRelayProofCase(fmt.Sprintf("mode-%d-capacity-axis-%d", mode, offset), "registration", changedInit.encode(), challenge, proof, challenge.expires-1, false))
		}
		old := init
		old.receiveCapacity = nil
		oldProof := relaySenderProof("WS2P", mode, r.f.edPublic, ed25519.Sign(r.f.edPrivate, old.preimage(domain, r.relayIdentity, challenge)))
		cases = append(cases, newRelayProofCase(fmt.Sprintf("mode-%d-capacity-not-signed", mode), "registration", init.encode(), challenge, oldProof, challenge.expires-1, false))
	}
	result["cases"] = cases
	return result
}
