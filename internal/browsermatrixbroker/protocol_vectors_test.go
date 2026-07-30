package browsermatrixbroker

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func TestCanonicalPublicProtocolVectors(t *testing.T) {
	signer := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x24}, ed25519.SeedSize))
	credential := []byte("D5_PIPE_PAYLOAD_0123456789_abcdef")
	leaseVectors := []struct {
		name          string
		value         LeasePayload
		wantDocument  string
		wantDigest    string
		wantSignature string
	}{
		{
			name: "public-stun lease",
			value: LeasePayload{
				ProtocolVersion: RemotePionProtocolVersion,
				RequestID:       "acquire-request-00000001", ReleaseRequestID: "release-request-00000001",
				RevokeRequestID: "revoke-request-00000001", LeaseID: "control-lease-00000001",
				RunID: "scheduled-run", ProfileID: "scheduled-public-stun", ProbeNonce: "probe-nonce-00000001",
				AuthorityInstanceID: "remote-authority", AttestationSHA256: string(bytes.Repeat([]byte{'a'}, 64)),
				IssuedAt: "2031-02-03T04:05:06.000Z", ExpiresAt: "2031-02-03T04:06:06.000Z",
				MaxAttempts: 1, CredentialByteLength: len(credential), TURNCapability: "not-required",
			},
			wantDocument:  `{"protocolVersion":"windshare.browser-network-matrix.remote-pion/v2","requestId":"acquire-request-00000001","releaseRequestId":"release-request-00000001","revokeRequestId":"revoke-request-00000001","leaseId":"control-lease-00000001","runId":"scheduled-run","profileId":"scheduled-public-stun","probeNonce":"probe-nonce-00000001","authorityInstanceId":"remote-authority","attestationSha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","issuedAt":"2031-02-03T04:05:06.000Z","expiresAt":"2031-02-03T04:06:06.000Z","maxAttempts":1,"credentialByteLength":33,"turnCapability":"not-required","turnProviderLeaseId":"","turnCredentialId":"","turnUsername":"","turnExpiresAt":""}` + "\n",
			wantDigest:    "8bed0d8ac432bf8de092c9658a6f9e65f0dff992c47aea3b381c37280e47a060",
			wantSignature: "84B1QE-pkotIuwh6_gaJzlAa6ma3nTb3npD_9UYGOu2rhQuVkSfmg4inh73IUSn6_QPDnh2cShEMbyD_fjzkCg",
		},
		{
			name: "coturn lease",
			value: LeasePayload{
				ProtocolVersion: RemotePionProtocolVersion,
				RequestID:       "acquire-request-00000001", ReleaseRequestID: "release-request-00000001",
				RevokeRequestID: "revoke-request-00000001", LeaseID: "control-lease-00000001",
				RunID: "scheduled-run", ProfileID: "scheduled-coturn", ProbeNonce: "probe-nonce-00000001",
				AuthorityInstanceID: "remote-authority", AttestationSHA256: string(bytes.Repeat([]byte{'a'}, 64)),
				IssuedAt: "2031-02-03T04:05:06.000Z", ExpiresAt: "2031-02-03T04:06:06.000Z",
				MaxAttempts: 1, CredentialByteLength: len(credential), TURNCapability: "bound",
				TURNProviderLeaseID: "provider-lease-00000001", TURNCredentialID: "dynamic-turn-credential",
				TURNUsername: "dynamic-turn-user", TURNExpiresAt: "2031-02-03T04:06:06.000Z",
			},
			wantDocument:  `{"protocolVersion":"windshare.browser-network-matrix.remote-pion/v2","requestId":"acquire-request-00000001","releaseRequestId":"release-request-00000001","revokeRequestId":"revoke-request-00000001","leaseId":"control-lease-00000001","runId":"scheduled-run","profileId":"scheduled-coturn","probeNonce":"probe-nonce-00000001","authorityInstanceId":"remote-authority","attestationSha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","issuedAt":"2031-02-03T04:05:06.000Z","expiresAt":"2031-02-03T04:06:06.000Z","maxAttempts":1,"credentialByteLength":33,"turnCapability":"bound","turnProviderLeaseId":"provider-lease-00000001","turnCredentialId":"dynamic-turn-credential","turnUsername":"dynamic-turn-user","turnExpiresAt":"2031-02-03T04:06:06.000Z"}` + "\n",
			wantDigest:    "48e640f20a3054afd70d4a41bc801ec7dfa3af44237e940ba745d21099c642a2",
			wantSignature: "djUbccEs-RDVORuEzM8D_KVMimJG8XSIaqfx7nGhuTZHUOtxwoTMamsxScB3QZK2NK4aCie5DHyDKql5SbOgDg",
		},
	}
	for _, vector := range leaseVectors {
		t.Run(vector.name, func(t *testing.T) {
			document, err := canonicalJSONLine(vector.value)
			if err != nil || string(document) != vector.wantDocument {
				t.Fatalf("canonical lease document differs: %q err=%v", document, err)
			}
			digest := sha256.Sum256(document)
			signature := ed25519.Sign(signer, document)
			if hex.EncodeToString(digest[:]) != vector.wantDigest ||
				base64.RawURLEncoding.EncodeToString(signature) != vector.wantSignature {
				t.Fatal("lease digest or signature differs from its deterministic vector")
			}
			frame, err := encodeLeaseFrame(signer, vector.value, credential)
			if err != nil {
				t.Fatal(err)
			}
			metadata, payload, err := splitFrame(frame)
			if err != nil || !bytes.Equal(payload, credential) || bytes.Contains(metadata, credential) {
				t.Fatal("D5 payload crossed the public signed metadata boundary")
			}
			var envelope leaseEnvelope
			if !decodeCanonicalMetadata(metadata, &envelope) ||
				envelope.LeaseSHA256 != hex.EncodeToString(digest[:]) ||
				envelope.Signature != base64.RawURLEncoding.EncodeToString(signature) {
				t.Fatal("lease envelope differs from its deterministic public vector")
			}
		})
	}

	receiptVectors := []struct {
		name          string
		value         ReceiptPayload
		wantDocument  string
		wantDigest    string
		wantSignature string
	}{
		{
			name: "public-stun receipt",
			value: ReceiptPayload{
				ProtocolVersion: RemotePionProtocolVersion, Operation: "release",
				RequestID: "release-request-00000001", ReleaseRequestID: "release-request-00000001",
				RevokeRequestID: "revoke-request-00000001", LeaseID: "control-lease-00000001",
				RunID: "scheduled-run", ProfileID: "scheduled-public-stun", ProbeNonce: "probe-nonce-00000001",
				AuthorityInstanceID: "remote-authority", AttestationSHA256: string(bytes.Repeat([]byte{'a'}, 64)),
				LeaseExpiresAt: "2031-02-03T04:06:06.000Z", ControlTerminal: "revoked",
				TURNTerminal: "not-required", Terminal: "revoked", RetiredAt: "2031-02-03T04:05:07.000Z",
			},
			wantDocument:  `{"protocolVersion":"windshare.browser-network-matrix.remote-pion/v2","operation":"release","requestId":"release-request-00000001","releaseRequestId":"release-request-00000001","revokeRequestId":"revoke-request-00000001","leaseId":"control-lease-00000001","runId":"scheduled-run","profileId":"scheduled-public-stun","probeNonce":"probe-nonce-00000001","authorityInstanceId":"remote-authority","attestationSha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","leaseExpiresAt":"2031-02-03T04:06:06.000Z","controlTerminal":"revoked","turnProviderLeaseId":"","turnTerminal":"not-required","terminal":"revoked","retiredAt":"2031-02-03T04:05:07.000Z"}` + "\n",
			wantDigest:    "2abbb0e38af6f2c748548eda10e4269dfee8cfb76116a11dd927b7f66809e84b",
			wantSignature: "Pv2WNs_f8dK0_COMekb1kgMZ89iQXXUcNe7wFK58DNpcq0WsXtLp9_XyRufXd9nmDOOJotyEkVyGPbQ10ap9DQ",
		},
		{
			name: "coturn receipt",
			value: ReceiptPayload{
				ProtocolVersion: RemotePionProtocolVersion, Operation: "revoke-and-wait",
				RequestID: "revoke-request-00000001", ReleaseRequestID: "release-request-00000001",
				RevokeRequestID: "revoke-request-00000001", LeaseID: "control-lease-00000001",
				RunID: "scheduled-run", ProfileID: "scheduled-coturn", ProbeNonce: "probe-nonce-00000001",
				AuthorityInstanceID: "remote-authority", AttestationSHA256: string(bytes.Repeat([]byte{'a'}, 64)),
				LeaseExpiresAt: "2031-02-03T04:06:06.000Z", ControlTerminal: "revoked",
				TURNProviderLeaseID: "provider-lease-00000001", TURNTerminal: "revoked",
				Terminal: "revoked", RetiredAt: "2031-02-03T04:05:07.000Z",
			},
			wantDocument:  `{"protocolVersion":"windshare.browser-network-matrix.remote-pion/v2","operation":"revoke-and-wait","requestId":"revoke-request-00000001","releaseRequestId":"release-request-00000001","revokeRequestId":"revoke-request-00000001","leaseId":"control-lease-00000001","runId":"scheduled-run","profileId":"scheduled-coturn","probeNonce":"probe-nonce-00000001","authorityInstanceId":"remote-authority","attestationSha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","leaseExpiresAt":"2031-02-03T04:06:06.000Z","controlTerminal":"revoked","turnProviderLeaseId":"provider-lease-00000001","turnTerminal":"revoked","terminal":"revoked","retiredAt":"2031-02-03T04:05:07.000Z"}` + "\n",
			wantDigest:    "72cdae7fd602baca08bbc6b525238ee0376f91512fba54aa7bd67db0e1b2564f",
			wantSignature: "Q2mC5fU-esCFeM94Vt21XvuquOUxzVPM29JDXMiOZo9hcEVIl6IKqpESIpDcFMWEQZCObLRv1EHyMEZaTSrBAA",
		},
	}
	for _, vector := range receiptVectors {
		t.Run(vector.name, func(t *testing.T) {
			document, err := canonicalJSONLine(vector.value)
			if err != nil || string(document) != vector.wantDocument {
				t.Fatalf("canonical receipt document differs: %q err=%v", document, err)
			}
			digest := sha256.Sum256(document)
			signature := ed25519.Sign(signer, document)
			if hex.EncodeToString(digest[:]) != vector.wantDigest ||
				base64.RawURLEncoding.EncodeToString(signature) != vector.wantSignature {
				t.Fatal("receipt digest or signature differs from its deterministic vector")
			}
			frame, err := encodeReceiptFrame(signer, vector.value)
			if err != nil {
				t.Fatal(err)
			}
			metadata, payload, err := splitFrame(frame)
			if err != nil || len(payload) != 0 {
				t.Fatal("receipt frame unexpectedly carried D5 payload")
			}
			var envelope receiptEnvelope
			if !decodeCanonicalMetadata(metadata, &envelope) ||
				envelope.ReceiptSHA256 != hex.EncodeToString(digest[:]) ||
				envelope.Signature != base64.RawURLEncoding.EncodeToString(signature) {
				t.Fatal("receipt envelope differs from its deterministic public vector")
			}
		})
	}
}
