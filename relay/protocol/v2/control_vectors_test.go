package v2

import (
	"testing"
	"time"
)

// Shared relay-control refusal vectors are also consumed by the browser codec
// and the Rust relay kernel; these tests keep the Go codec in the same contract.

var controlFrameDecoders = map[string]func([]byte) error{
	"WS2R": func(raw []byte) error { _, err := ParseRegisterInit(raw); return err },
	"WS2Q": func(raw []byte) error { _, err := ParseChallenge(raw); return err },
	"WS2P": func(raw []byte) error { _, err := ParseRegisterProof(raw); return err },
	"WS2T": func(raw []byte) error { _, err := ParseResumeCredential(raw); return err },
	"WS2U": func(raw []byte) error { _, err := ParseDescriptorUpload(raw); return err },
	"WS2K": func(raw []byte) error { _, err := ParseRegistered(raw); return err },
	"WS2X": func(raw []byte) error { _, err := ParseStopInit(raw); return err },
	"WS2V": func(raw []byte) error { _, err := ParseStopProof(raw); return err },
	"WS2Y": func(raw []byte) error { _, err := ParseStopped(raw); return err },
	"WS2E": func(raw []byte) error { _, err := ParseError(raw); return err },
}

func TestControlFramesFollowSharedDecodingVectors(t *testing.T) {
	var vector struct {
		Cases []struct {
			Name       string `json:"name"`
			Frame      string `json:"frame"`
			EncodedB64 string `json:"encodedB64"`
			Accepted   bool   `json:"accepted"`
		} `json:"cases"`
	}
	loadV2VectorCase(t, "v2-session.json", "relay-control-frame-decoding", &vector)
	for _, test := range vector.Cases {
		t.Run(test.Name, func(t *testing.T) {
			decode, ok := controlFrameDecoders[test.Frame]
			if !ok {
				t.Fatalf("no Go decoder owns %s", test.Frame)
			}
			if err := decode(testB64(t, test.EncodedB64)); (err == nil) != test.Accepted {
				t.Fatalf("accepted=%t error=%v", test.Accepted, err)
			}
		})
	}
}

func TestProofsFollowSharedAuthenticationVectors(t *testing.T) {
	var vector struct {
		RelayIdentityB64 string `json:"relayIdentityB64"`
		Cases            []struct {
			Name               string           `json:"name"`
			Operation          string           `json:"operation"`
			InitB64            string           `json:"initB64"`
			ChallengePurpose   ChallengePurpose `json:"challengePurpose"`
			ChallengeIDB64     string           `json:"challengeIdB64"`
			ChallengeNonceB64  string           `json:"challengeNonceB64"`
			ChallengeExpiresAt string           `json:"challengeExpiresAt"`
			ProofB64           string           `json:"proofB64"`
			NowUnixSeconds     string           `json:"nowUnixSeconds"`
			Authenticated      bool             `json:"authenticated"`
		} `json:"cases"`
	}
	loadV2VectorCase(t, "v2-session.json", "relay-control-proof-authentication", &vector)
	var relay RelayIdentity
	copy(relay[:], testB64(t, vector.RelayIdentityB64))
	for _, test := range vector.Cases {
		t.Run(test.Name, func(t *testing.T) {
			challenge := Challenge{
				Purpose: test.ChallengePurpose, ExpiresAtUnixSeconds: parseVectorUint64(t, test.ChallengeExpiresAt),
			}
			copy(challenge.ID[:], testB64(t, test.ChallengeIDB64))
			copy(challenge.Nonce[:], testB64(t, test.ChallengeNonceB64))
			now := time.Unix(int64(parseVectorUint64(t, test.NowUnixSeconds)), 0)
			var err error
			switch test.Operation {
			case "registration":
				init, initErr := ParseRegisterInit(testB64(t, test.InitB64))
				proof, proofErr := ParseRegisterProof(testB64(t, test.ProofB64))
				if initErr != nil || proofErr != nil {
					t.Fatalf("proof vectors carry valid frames: %v, %v", initErr, proofErr)
				}
				_, err = authenticateRegisterProof(init, challenge, relay, proof, now)
			case "stop":
				init, initErr := ParseStopInit(testB64(t, test.InitB64))
				proof, proofErr := ParseStopProof(testB64(t, test.ProofB64))
				if initErr != nil || proofErr != nil {
					t.Fatalf("proof vectors carry valid frames: %v, %v", initErr, proofErr)
				}
				_, err = authenticateStopProof(init, challenge, relay, proof, now)
			default:
				t.Fatalf("unknown operation %q", test.Operation)
			}
			if (err == nil) != test.Authenticated {
				t.Fatalf("authenticated=%t error=%v", test.Authenticated, err)
			}
		})
	}
}
