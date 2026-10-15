package protocolcontract

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"slices"
)

// Relay control vectors pin every refusal a relay implementation must
// reproduce. Hostile bytes are derived here from canonical layouts rather than
// from a production codec, so Go, browser and Rust decoders cannot agree on a
// shared mistake. Decoding cases isolate structural refusals; proof cases use
// structurally valid frames so an authority failure never hides behind a
// decoding failure.

const (
	relayClientToRelay = "client-to-relay"
	relayRelayToClient = "relay-to-client"

	relayModeFresh       = 0
	relayModeResume      = 1
	relayPurposeRegister = 0
	relayPurposeResume   = 1
	relayPurposeStop     = 2

	relayErrorAdmission      = 10
	relayErrorStopped        = 11
	relayErrorLastCode       = 12
	relayRetryLimitMillis    = 30_000
	relayPermanentRetryProbe = 1_000

	relayHeaderBytes         = 8
	relayShareIDBytes        = 12
	relayShareInstanceBytes  = 16
	relayPKHashBytes         = 16
	relayIdentityBytes       = 32
	relayChallengeIDBytes    = 16
	relayChallengeNonceBytes = 32
	relayExpiryBytes         = 8
	relaySenderKeyBytes      = 32
	relaySignatureBytes      = 64
	relayResumeTokenBytes    = 32
	relayStopIDBytes         = 16
)

type relayControlFixture struct {
	f             *fixture
	descriptor    []byte
	relayIdentity []byte
	resumeHash    []byte
	expires       uint64
}

type relayChallenge struct {
	purpose   byte
	id, nonce []byte
	expires   uint64
}

func (c relayChallenge) encode() []byte {
	return slices.Concat([]byte("WS2Q"), []byte{wireVersion, c.purpose, 0, 0}, c.signedFields())
}

func (c relayChallenge) signedFields() []byte { return slices.Concat(c.id, c.nonce, u64(c.expires)) }

func (c relayChallenge) withPurpose(purpose byte) relayChallenge {
	c.purpose = purpose
	return c
}

type relayRegistration struct {
	mode                                               byte
	shareID, shareInstance, pkHash, digest, resumeHash []byte
}

func (r relayRegistration) encode() []byte {
	return slices.Concat(
		[]byte("WS2R"), []byte{wireVersion, r.mode, 0, 0},
		r.shareID, r.shareInstance, r.pkHash, r.digest, r.resumeHash,
	)
}

func (r relayRegistration) preimage(domain string, relayIdentity []byte, c relayChallenge) []byte {
	return slices.Concat(
		[]byte(domain), r.shareID, r.shareInstance, r.pkHash, r.digest, r.resumeHash,
		relayIdentity, c.signedFields(),
	)
}

type relayStop struct {
	shareID, shareInstance, pkHash, relayIdentity, stopID []byte
}

func (s relayStop) encode() []byte {
	return slices.Concat(
		[]byte("WS2X"), []byte{wireVersion, 0, 0, 0},
		s.shareID, s.shareInstance, s.pkHash, s.relayIdentity, s.stopID,
	)
}

func (s relayStop) preimage(c relayChallenge) []byte {
	return slices.Concat(
		[]byte(domainStop), s.shareID, s.shareInstance, s.pkHash, s.relayIdentity, s.stopID,
		c.signedFields(),
	)
}

func relaySenderProof(magic string, discriminator byte, publicKey, signature []byte) []byte {
	return slices.Concat([]byte(magic), []byte{wireVersion, discriminator, 0, 0}, publicKey, signature)
}

func relayErrorFrame(code uint16, retryMilliseconds uint32) []byte {
	return slices.Concat([]byte("WS2E"), []byte{wireVersion, 0}, u16(code), u32(retryMilliseconds))
}

func (r relayControlFixture) registration(mode byte) relayRegistration {
	return relayRegistration{
		mode: mode, shareID: r.f.shareIDRaw, shareInstance: r.f.shareInstance,
		pkHash: r.f.pkHash, digest: hash(r.descriptor), resumeHash: r.resumeHash,
	}
}

// Challenge values repeat fresh-relay-registration-proof so both vector
// families describe the same relay conversation.
func (r relayControlFixture) challenge(purpose byte) relayChallenge {
	switch purpose {
	case relayPurposeResume:
		return relayChallenge{purpose: purpose, id: fixed(0x02, relayChallengeIDBytes), nonce: fixed(0x42, relayChallengeNonceBytes), expires: r.expires + 1}
	case relayPurposeStop:
		return relayChallenge{purpose: purpose, id: fixed(0x72, relayChallengeIDBytes), nonce: fixed(0x82, relayChallengeNonceBytes), expires: r.expires + 2}
	default:
		return relayChallenge{purpose: purpose, id: fixed(0x01, relayChallengeIDBytes), nonce: fixed(0x21, relayChallengeNonceBytes), expires: r.expires}
	}
}

func (r relayControlFixture) stop() relayStop {
	return relayStop{
		shareID: r.f.shareIDRaw, shareInstance: r.f.shareInstance, pkHash: r.f.pkHash,
		relayIdentity: r.relayIdentity, stopID: fixed(0x62, relayStopIDBytes),
	}
}

type relayDecodingCase struct {
	Name       string `json:"name"`
	Frame      string `json:"frame"`
	Direction  string `json:"direction"`
	EncodedB64 string `json:"encodedB64"`
	Accepted   bool   `json:"accepted"`
}

type relayFrameSample struct {
	label     string
	direction string
	canonical []byte
	// Header bytes that must stay zero. Byte 5 is a discriminator for WS2R,
	// WS2Q and WS2P; WS2E keeps only byte 5 reserved because its code follows.
	reserved []int
	variants []relayFrameVariant
}

type relayFrameVariant struct {
	name     string
	encoded  []byte
	accepted bool
}

func rejectedFrame(name string, encoded []byte) relayFrameVariant {
	return relayFrameVariant{name: name, encoded: encoded}
}

func changed[T any](value T, change func(*T)) T {
	change(&value)
	return value
}

func withByte(encoded []byte, offset int, value byte) []byte {
	result := bytes.Clone(encoded)
	result[offset] = value
	return result
}

func withFlippedByte(encoded []byte, offset int) []byte {
	return withByte(encoded, offset, encoded[offset]^1)
}

func withZeroField(encoded []byte, offset, width int) []byte {
	result := bytes.Clone(encoded)
	clear(result[offset : offset+width])
	return result
}

func relayControlFrameDecodingVectors(r relayControlFixture) map[string]any {
	fresh, freshChallenge := r.registration(relayModeFresh), r.challenge(relayPurposeRegister)
	stop, stopChallenge := r.stop(), r.challenge(relayPurposeStop)
	register := fresh.encode()
	challenge := freshChallenge.encode()
	registerProof := relaySenderProof(
		"WS2P", relayModeFresh, r.f.edPublic,
		ed25519.Sign(r.f.edPrivate, fresh.preimage(domainRegister, r.relayIdentity, freshChallenge)),
	)
	credential := slices.Concat([]byte("WS2T"), []byte{wireVersion, 0, 0, 0}, fixed(0xa1, relayResumeTokenBytes))
	registered := slices.Concat(
		[]byte("WS2K"), []byte{wireVersion, 0, 0, 0}, fresh.shareID, fresh.shareInstance, fresh.digest,
	)
	stopInit := stop.encode()
	stopProof := relaySenderProof("WS2V", 0, r.f.edPublic, ed25519.Sign(r.f.edPrivate, stop.preimage(stopChallenge)))
	stopped := slices.Concat([]byte("WS2Y"), []byte{wireVersion, 0, 0, 0}, stop.stopID)

	shareInstanceAt := relayHeaderBytes + relayShareIDBytes
	pkHashAt := shareInstanceAt + relayShareInstanceBytes
	stopRelayAt := pkHashAt + relayPKHashBytes
	stopIDAt := stopRelayAt + relayIdentityBytes
	nonceAt := relayHeaderBytes + relayChallengeIDBytes
	expiryAt := nonceAt + relayChallengeNonceBytes
	signatureAt := relayHeaderBytes + relaySenderKeyBytes
	controlReserved, headerReserved := []int{6, 7}, []int{5, 6, 7}

	samples := []relayFrameSample{
		{label: "register-init", direction: relayClientToRelay, canonical: register, reserved: controlReserved, variants: []relayFrameVariant{
			rejectedFrame("unknown-mode", withByte(register, 5, relayModeResume+1)),
			rejectedFrame("zero-share-instance", withZeroField(register, shareInstanceAt, relayShareInstanceBytes)),
			rejectedFrame("share-id-not-derived-from-pk-hash", withFlippedByte(register, relayHeaderBytes)),
			rejectedFrame("pk-hash-not-deriving-share-id", withFlippedByte(register, pkHashAt)),
		}},
		{label: "challenge", direction: relayRelayToClient, canonical: challenge, reserved: controlReserved, variants: []relayFrameVariant{
			rejectedFrame("unknown-purpose", withByte(challenge, 5, relayPurposeStop+1)),
			rejectedFrame("zero-id", withZeroField(challenge, relayHeaderBytes, relayChallengeIDBytes)),
			rejectedFrame("zero-nonce", withZeroField(challenge, nonceAt, relayChallengeNonceBytes)),
			rejectedFrame("zero-expiry", withZeroField(challenge, expiryAt, relayExpiryBytes)),
		}},
		{label: "register-proof", direction: relayClientToRelay, canonical: registerProof, reserved: controlReserved, variants: []relayFrameVariant{
			rejectedFrame("unknown-mode", withByte(registerProof, 5, relayModeResume+1)),
			rejectedFrame("zero-sender-key", withZeroField(registerProof, relayHeaderBytes, relaySenderKeyBytes)),
			rejectedFrame("zero-signature", withZeroField(registerProof, signatureAt, relaySignatureBytes)),
		}},
		{label: "resume-credential", direction: relayClientToRelay, canonical: credential, reserved: headerReserved, variants: []relayFrameVariant{
			rejectedFrame("zero-token", withZeroField(credential, relayHeaderBytes, relayResumeTokenBytes)),
		}},
		{label: "descriptor-upload", direction: relayClientToRelay, canonical: encodeDescriptorUpload(r.descriptor), reserved: headerReserved, variants: []relayFrameVariant{
			rejectedFrame("empty-object", encodeDescriptorUpload(nil)),
			{name: "maximum-object", encoded: encodeDescriptorUpload(bytes.Repeat([]byte{0x5a}, maxDescriptorBytes)), accepted: true},
			rejectedFrame("oversized-object", encodeDescriptorUpload(bytes.Repeat([]byte{0x5a}, maxDescriptorBytes+1))),
		}},
		{label: "registered", direction: relayRelayToClient, canonical: registered, reserved: headerReserved, variants: []relayFrameVariant{
			rejectedFrame("zero-share-id", withZeroField(registered, relayHeaderBytes, relayShareIDBytes)),
			rejectedFrame("zero-share-instance", withZeroField(registered, shareInstanceAt, relayShareInstanceBytes)),
		}},
		{label: "stop-init", direction: relayClientToRelay, canonical: stopInit, reserved: headerReserved, variants: []relayFrameVariant{
			rejectedFrame("zero-share-instance", withZeroField(stopInit, shareInstanceAt, relayShareInstanceBytes)),
			rejectedFrame("zero-relay-identity", withZeroField(stopInit, stopRelayAt, relayIdentityBytes)),
			rejectedFrame("zero-stop-id", withZeroField(stopInit, stopIDAt, relayStopIDBytes)),
			rejectedFrame("share-id-not-derived-from-pk-hash", withFlippedByte(stopInit, relayHeaderBytes)),
		}},
		{label: "stop-proof", direction: relayClientToRelay, canonical: stopProof, reserved: headerReserved, variants: []relayFrameVariant{
			rejectedFrame("zero-sender-key", withZeroField(stopProof, relayHeaderBytes, relaySenderKeyBytes)),
			rejectedFrame("zero-signature", withZeroField(stopProof, signatureAt, relaySignatureBytes)),
		}},
		{label: "stopped", direction: relayRelayToClient, canonical: stopped, reserved: headerReserved, variants: []relayFrameVariant{
			rejectedFrame("zero-stop-id", withZeroField(stopped, relayHeaderBytes, relayStopIDBytes)),
		}},
		{label: "error", direction: relayRelayToClient, canonical: relayErrorFrame(relayErrorStopped, 0), reserved: []int{5}, variants: []relayFrameVariant{
			rejectedFrame("unknown-code-zero", relayErrorFrame(0, 0)),
			rejectedFrame("unknown-code-after-last", relayErrorFrame(relayErrorLastCode+1, 0)),
			rejectedFrame("retry-on-permanent-refusal", relayErrorFrame(relayErrorStopped, relayPermanentRetryProbe)),
			rejectedFrame("retry-beyond-limit", relayErrorFrame(relayErrorAdmission, relayRetryLimitMillis+1)),
			{name: "retry-at-limit", encoded: relayErrorFrame(relayErrorAdmission, relayRetryLimitMillis), accepted: true},
		}},
	}
	var cases []relayDecodingCase
	for _, sample := range samples {
		cases = append(cases, relayFrameCases(sample)...)
	}
	return map[string]any{"name": "relay-control-frame-decoding", "cases": cases}
}

func relayFrameCases(sample relayFrameSample) []relayDecodingCase {
	canonical := sample.canonical
	variants := []relayFrameVariant{
		{name: "canonical", encoded: canonical, accepted: true},
		rejectedFrame("truncated", canonical[:len(canonical)-1]),
		rejectedFrame("trailing-byte", append(bytes.Clone(canonical), 0)),
		// Lowercasing the last magic letter yields a magic no decoder owns.
		rejectedFrame("foreign-magic", withByte(canonical, 3, canonical[3]^0x20)),
		rejectedFrame("next-wire-version", withByte(canonical, 4, wireVersion+1)),
	}
	for _, offset := range sample.reserved {
		variants = append(variants, rejectedFrame(fmt.Sprintf("reserved-byte-%d", offset), withByte(canonical, offset, 1)))
	}
	variants = append(variants, sample.variants...)
	cases := make([]relayDecodingCase, 0, len(variants))
	for _, variant := range variants {
		cases = append(cases, relayDecodingCase{
			Name: sample.label + "/" + variant.name, Frame: string(canonical[:4]), Direction: sample.direction,
			EncodedB64: b64(variant.encoded), Accepted: variant.accepted,
		})
	}
	return cases
}

type relayProofCase struct {
	Name               string `json:"name"`
	Operation          string `json:"operation"`
	InitB64            string `json:"initB64"`
	ChallengePurpose   byte   `json:"challengePurpose"`
	ChallengeIDB64     string `json:"challengeIdB64"`
	ChallengeNonceB64  string `json:"challengeNonceB64"`
	ChallengeExpiresAt string `json:"challengeExpiresAt"`
	ProofB64           string `json:"proofB64"`
	NowUnixSeconds     string `json:"nowUnixSeconds"`
	Authenticated      bool   `json:"authenticated"`
}

func newRelayProofCase(name, operation string, init []byte, challenge relayChallenge, proof []byte, now uint64, authenticated bool) relayProofCase {
	return relayProofCase{
		Name: name, Operation: operation, InitB64: b64(init), ChallengePurpose: challenge.purpose,
		ChallengeIDB64: b64(challenge.id), ChallengeNonceB64: b64(challenge.nonce),
		ChallengeExpiresAt: fmt.Sprint(challenge.expires), ProofB64: b64(proof),
		NowUnixSeconds: fmt.Sprint(now), Authenticated: authenticated,
	}
}

func relayControlProofVectors(r relayControlFixture) map[string]any {
	sign := func(preimage []byte) []byte { return ed25519.Sign(r.f.edPrivate, preimage) }
	foreign := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x91}, ed25519.SeedSize))
	foreignPublic := foreign.Public().(ed25519.PublicKey)
	otherRelay := flipped(r.relayIdentity)

	fresh, freshChallenge := r.registration(relayModeFresh), r.challenge(relayPurposeRegister)
	freshPreimage := fresh.preimage(domainRegister, r.relayIdentity, freshChallenge)
	freshSignature := sign(freshPreimage)
	resume, resumeChallenge := r.registration(relayModeResume), r.challenge(relayPurposeResume)
	resumeSignature := sign(resume.preimage(domainResume, r.relayIdentity, resumeChallenge))
	stop, stopChallenge := r.stop(), r.challenge(relayPurposeStop)
	stopPreimage := stop.preimage(stopChallenge)
	stopSignature := sign(stopPreimage)

	registerProof := func(mode byte, publicKey, signature []byte) []byte {
		return relaySenderProof("WS2P", mode, publicKey, signature)
	}
	stopProof := func(publicKey, signature []byte) []byte { return relaySenderProof("WS2V", 0, publicKey, signature) }
	freshProof := registerProof(relayModeFresh, r.f.edPublic, freshSignature)
	resumeProof := registerProof(relayModeResume, r.f.edPublic, resumeSignature)
	validStopProof := stopProof(r.f.edPublic, stopSignature)
	freshLive, resumeLive, stopLive := freshChallenge.expires-1, resumeChallenge.expires-1, stopChallenge.expires-1

	registration := func(name string, init relayRegistration, challenge relayChallenge, proof []byte, now uint64, authenticated bool) relayProofCase {
		return newRelayProofCase(name, "registration", init.encode(), challenge, proof, now, authenticated)
	}
	stopping := func(name string, init relayStop, challenge relayChallenge, proof []byte, now uint64, authenticated bool) relayProofCase {
		return newRelayProofCase(name, "stop", init.encode(), challenge, proof, now, authenticated)
	}
	otherRelayStop := changed(stop, func(value *relayStop) { value.relayIdentity = otherRelay })

	// Substitution cases change one signed axis after signing, so only a relay
	// that binds that axis into its preimage refuses them.
	cases := []relayProofCase{
		registration("fresh-at-last-live-second", fresh, freshChallenge, freshProof, freshLive, true),
		registration("fresh-at-expiry", fresh, freshChallenge, freshProof, freshChallenge.expires, false),
		registration("resume-at-last-live-second", resume, resumeChallenge, resumeProof, resumeLive, true),
		registration("fresh-proof-claims-resume", fresh, freshChallenge,
			registerProof(relayModeResume, r.f.edPublic, freshSignature), freshLive, false),
		registration("resume-proof-claims-fresh", resume, resumeChallenge,
			registerProof(relayModeFresh, r.f.edPublic, resumeSignature), resumeLive, false),
		registration("fresh-answers-resume-challenge", fresh, freshChallenge.withPurpose(relayPurposeResume), freshProof, freshLive, false),
		registration("resume-answers-register-challenge", resume, resumeChallenge.withPurpose(relayPurposeRegister), resumeProof, resumeLive, false),
		registration("fresh-signed-with-resume-domain", fresh, freshChallenge, registerProof(
			relayModeFresh, r.f.edPublic, sign(fresh.preimage(domainResume, r.relayIdentity, freshChallenge))), freshLive, false),
		registration("resume-signed-with-register-domain", resume, resumeChallenge, registerProof(
			relayModeResume, r.f.edPublic, sign(resume.preimage(domainRegister, r.relayIdentity, resumeChallenge))), resumeLive, false),
		registration("fresh-signed-for-another-relay", fresh, freshChallenge, registerProof(
			relayModeFresh, r.f.edPublic, sign(fresh.preimage(domainRegister, otherRelay, freshChallenge))), freshLive, false),
		registration("fresh-signed-by-foreign-key", fresh, freshChallenge,
			registerProof(relayModeFresh, foreignPublic, ed25519.Sign(foreign, freshPreimage)), freshLive, false),
		registration("fresh-signature-substitution", fresh, freshChallenge,
			registerProof(relayModeFresh, r.f.edPublic, flipped(freshSignature)), freshLive, false),
		registration("fresh-share-instance-substitution", changed(fresh, func(value *relayRegistration) {
			value.shareInstance = flipped(value.shareInstance)
		}), freshChallenge, freshProof, freshLive, false),
		registration("fresh-descriptor-digest-substitution", changed(fresh, func(value *relayRegistration) {
			value.digest = flipped(value.digest)
		}), freshChallenge, freshProof, freshLive, false),
		registration("fresh-resume-token-hash-substitution", changed(fresh, func(value *relayRegistration) {
			value.resumeHash = flipped(value.resumeHash)
		}), freshChallenge, freshProof, freshLive, false),
		registration("fresh-challenge-id-substitution", fresh, changed(freshChallenge, func(value *relayChallenge) {
			value.id = flipped(value.id)
		}), freshProof, freshLive, false),
		registration("fresh-challenge-nonce-substitution", fresh, changed(freshChallenge, func(value *relayChallenge) {
			value.nonce = flipped(value.nonce)
		}), freshProof, freshLive, false),
		registration("fresh-challenge-expiry-substitution", fresh, changed(freshChallenge, func(value *relayChallenge) {
			value.expires++
		}), freshProof, freshLive, false),
		registration("fresh-replays-stop-signature", fresh, freshChallenge,
			registerProof(relayModeFresh, r.f.edPublic, stopSignature), freshLive, false),
		stopping("stop-at-last-live-second", stop, stopChallenge, validStopProof, stopLive, true),
		stopping("stop-at-expiry", stop, stopChallenge, validStopProof, stopChallenge.expires, false),
		stopping("stop-answers-register-challenge", stop, stopChallenge.withPurpose(relayPurposeRegister), validStopProof, stopLive, false),
		stopping("stop-addressed-to-another-relay", otherRelayStop, stopChallenge,
			stopProof(r.f.edPublic, sign(otherRelayStop.preimage(stopChallenge))), stopLive, false),
		stopping("stop-id-substitution", changed(stop, func(value *relayStop) {
			value.stopID = flipped(value.stopID)
		}), stopChallenge, validStopProof, stopLive, false),
		stopping("stop-share-instance-substitution", changed(stop, func(value *relayStop) {
			value.shareInstance = flipped(value.shareInstance)
		}), stopChallenge, validStopProof, stopLive, false),
		stopping("stop-challenge-nonce-substitution", stop, changed(stopChallenge, func(value *relayChallenge) {
			value.nonce = flipped(value.nonce)
		}), validStopProof, stopLive, false),
		stopping("stop-signed-by-foreign-key", stop, stopChallenge,
			stopProof(foreignPublic, ed25519.Sign(foreign, stopPreimage)), stopLive, false),
		stopping("stop-signature-substitution", stop, stopChallenge,
			stopProof(r.f.edPublic, flipped(stopSignature)), stopLive, false),
		stopping("stop-replays-registration-signature", stop, stopChallenge,
			stopProof(r.f.edPublic, freshSignature), stopLive, false),
	}
	return map[string]any{
		"name": "relay-control-proof-authentication", "relayIdentityB64": b64(r.relayIdentity), "cases": cases,
	}
}
