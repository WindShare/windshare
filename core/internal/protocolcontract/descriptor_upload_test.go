package protocolcontract

import (
	"bytes"
	"crypto/ed25519"
	"slices"
)

// Relay publication and receiver object validity are independent contracts.
// Keeping both outcomes in one vector prevents a relay port from importing
// receiver parsing rules merely to reproduce the previous relay behavior.
func descriptorUploadAuthorizationVectors(f *fixture, descriptor, relayIdentity, resumeHash []byte, expires uint64) map[string]any {
	previousVersion := bytes.Clone(descriptor)
	previousVersion[0] = senderObjectWireVersion - 1
	invalidFlags := bytes.Clone(descriptor)
	invalidFlags[1] = 1
	invalidSignature := bytes.Clone(descriptor)
	invalidSignature[len(invalidSignature)-1] ^= 1
	opaque := []byte{0xff}
	cases := []struct {
		name          string
		committed     []byte
		uploaded      []byte
		receiverValid bool
	}{
		{"valid-object", descriptor, descriptor, true},
		{"opaque-byte", opaque, opaque, false},
		{"previous-object-version", previousVersion, previousVersion, false},
		{"invalid-object-flags", invalidFlags, invalidFlags, false},
		{"invalid-object-signature", invalidSignature, invalidSignature, false},
		{"digest-substitution", descriptor, invalidSignature, false},
	}
	challengeID, challengeNonce := fixed(0x01, 16), fixed(0x21, 32)
	challenge := slices.Concat(
		[]byte("WS2Q"), []byte{wireVersion, 0, 0, 0}, challengeID, challengeNonce, u64(expires),
	)
	vectors := make([]any, 0, len(cases))
	for _, test := range cases {
		digest := hash(test.committed)
		init := slices.Concat(
			[]byte("WS2R"), []byte{wireVersion, 0, 0, 0}, f.shareIDRaw, f.shareInstance,
			f.pkHash, digest, resumeHash,
		)
		preimage := slices.Concat(
			[]byte(domainRegister), f.shareIDRaw, f.shareInstance, f.pkHash, digest,
			resumeHash, relayIdentity, challengeID, challengeNonce, u64(expires),
		)
		proof := slices.Concat(
			[]byte("WS2P"), []byte{wireVersion, 0, 0, 0}, f.edPublic, ed25519.Sign(f.edPrivate, preimage),
		)
		vectors = append(vectors, map[string]any{
			"name": test.name, "registerInitB64": b64(init),
			"preimageB64": b64(preimage), "registerProofB64": b64(proof),
			"descriptorUploadB64":   b64(encodeDescriptorUpload(test.uploaded)),
			"relayUploadAuthorized": bytes.Equal(test.committed, test.uploaded),
			"receiverObjectValid":   test.receiverValid,
		})
	}
	return map[string]any{
		"name": "relay-descriptor-upload-authorization", "relayIdentityB64": b64(relayIdentity),
		"registerChallengeB64": b64(challenge), "descriptorKeyB64": b64(f.descriptorKey),
		"cases": vectors,
	}
}
