package v2

import (
	"bytes"
	"github.com/windshare/windshare/core/senderauth"
	"testing"
)

func TestDeliveryHandshakeSharedFrames(t *testing.T) {
	var vector struct {
		Cases []struct {
			Name, Frame, EncodedB64 string
			Accepted                bool
		}
	}
	loadV2VectorCase(t, "v2-session.json", "relay-capacity-frame-decoding", &vector)
	for _, test := range vector.Cases {
		t.Run(test.Name, func(t *testing.T) {
			raw := testB64(t, test.EncodedB64)
			var encoded []byte
			var err error
			switch test.Frame {
			case "WS2R":
				var frame DeliveryRegisterInit
				frame, err = ParseDeliveryRegisterInit(raw)
				if err == nil {
					encoded, err = frame.MarshalBinary()
				}
			case "WS2J":
				var frame DeliveryJoin
				frame, err = ParseDeliveryJoin(raw)
				if err == nil {
					encoded, err = frame.MarshalBinary()
				}
			default:
				t.Fatalf("unexpected frame %s", test.Frame)
			}
			if (err == nil) != test.Accepted {
				t.Fatalf("accepted=%v error=%v", test.Accepted, err)
			}
			if test.Accepted && !bytes.Equal(encoded, raw) {
				t.Fatal("handshake round trip changed wire bytes")
			}
		})
	}
}

func TestDeliveryHandshakeSharedPreimages(t *testing.T) {
	var vector struct{ RegisterInitB64, RegisterChallengeB64, PreimageB64, ResumeInitB64, ResumeChallengeB64, ResumePreimageB64, RelayIdentityB64 string }
	loadV2VectorCase(t, "v2-session.json", "relay-capacity-registration", &vector)
	var relay RelayIdentity
	copy(relay[:], testB64(t, vector.RelayIdentityB64))
	for _, test := range [][3]string{
		{vector.RegisterInitB64, vector.RegisterChallengeB64, vector.PreimageB64},
		{vector.ResumeInitB64, vector.ResumeChallengeB64, vector.ResumePreimageB64},
	} {
		init, err := ParseDeliveryRegisterInit(testB64(t, test[0]))
		if err != nil {
			t.Fatal(err)
		}
		challenge, err := ParseChallenge(testB64(t, test[1]))
		if err != nil {
			t.Fatal(err)
		}
		preimage, err := DeliveryRegistrationPreimage(init, challenge, relay)
		if err != nil || !bytes.Equal(preimage, testB64(t, test[2])) {
			t.Fatalf("preimage mismatch: %v", err)
		}
	}
}

func TestDeliveryHandshakeSharedProofs(t *testing.T) {
	var vector struct {
		RelayIdentityB64 string
		Cases            []struct {
			Name, Operation, InitB64, ChallengeIDB64, ChallengeNonceB64, ChallengeExpiresAt, ProofB64, NowUnixSeconds string
			ChallengePurpose                                                                                          ChallengePurpose
			Authenticated                                                                                             bool
		}
	}
	loadV2VectorCase(t, "v2-session.json", "relay-capacity-proof-authentication", &vector)
	var relay RelayIdentity
	copy(relay[:], testB64(t, vector.RelayIdentityB64))
	for _, test := range vector.Cases {
		if test.Operation != "registration" {
			continue
		}
		t.Run(test.Name, func(t *testing.T) {
			init, err := ParseDeliveryRegisterInit(testB64(t, test.InitB64))
			if err != nil {
				t.Fatal(err)
			}
			proof, err := ParseRegisterProof(testB64(t, test.ProofB64))
			if err != nil {
				t.Fatal(err)
			}
			challenge := Challenge{Purpose: test.ChallengePurpose, ExpiresAtUnixSeconds: parseVectorUint64(t, test.ChallengeExpiresAt)}
			copy(challenge.ID[:], testB64(t, test.ChallengeIDB64))
			copy(challenge.Nonce[:], testB64(t, test.ChallengeNonceB64))
			preimage, err := DeliveryRegistrationPreimage(init, challenge, relay)
			hash, keyErr := senderKeyHash(proof.SenderPublicKey[:])
			authenticated := err == nil && keyErr == nil && hash == init.Registration.PKHash &&
				init.Registration.Mode == proof.Mode && parseVectorUint64(t, test.NowUnixSeconds) < challenge.ExpiresAtUnixSeconds &&
				senderauth.Verify(proof.SenderPublicKey[:], preimage, proof.Signature[:])
			if authenticated != test.Authenticated {
				t.Fatalf("authenticated=%v wanted=%v", authenticated, test.Authenticated)
			}
		})
	}
}

func TestDeliveryHandshakeRejectsZeroCapacityOnEncode(t *testing.T) {
	for _, capacity := range []ReceiveCapacity{{}, {Frames: 1}, {EncodedBytes: 1}} {
		if _, err := (DeliveryRegisterInit{ReceiveCapacity: capacity}).MarshalBinary(); err == nil {
			t.Fatal("zero registration capacity accepted")
		}
		if _, err := (DeliveryJoin{ReceiveCapacity: capacity}).MarshalBinary(); err == nil {
			t.Fatal("zero join capacity accepted")
		}
	}
}
