package v2

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/catalogflow"
)

func TestDescriptorUploadAuthorizationAndReceiverValidityVectors(t *testing.T) {
	var vector struct {
		RelayIdentityB64     string `json:"relayIdentityB64"`
		RegisterChallengeB64 string `json:"registerChallengeB64"`
		DescriptorKeyB64     string `json:"descriptorKeyB64"`
		Cases                []struct {
			Name                  string `json:"name"`
			RegisterInitB64       string `json:"registerInitB64"`
			PreimageB64           string `json:"preimageB64"`
			RegisterProofB64      string `json:"registerProofB64"`
			DescriptorUploadB64   string `json:"descriptorUploadB64"`
			RelayUploadAuthorized bool   `json:"relayUploadAuthorized"`
			ReceiverObjectValid   bool   `json:"receiverObjectValid"`
		} `json:"cases"`
	}
	loadV2VectorCase(t, "v2-session.json", "relay-descriptor-upload-authorization", &vector)
	var relay RelayIdentity
	copy(relay[:], testB64(t, vector.RelayIdentityB64))
	challenge, err := ParseChallenge(testB64(t, vector.RegisterChallengeB64))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(int64(challenge.ExpiresAtUnixSeconds)-1, 0)
	for _, test := range vector.Cases {
		t.Run(test.Name, func(t *testing.T) {
			init, err := ParseRegisterInit(testB64(t, test.RegisterInitB64))
			if err != nil {
				t.Fatal(err)
			}
			proof, err := ParseRegisterProof(testB64(t, test.RegisterProofB64))
			if err != nil {
				t.Fatal(err)
			}
			preimage, err := RegistrationPreimage(init, challenge, relay)
			if err != nil || !bytes.Equal(preimage, testB64(t, test.PreimageB64)) {
				t.Fatalf("registration preimage mismatch: %v", err)
			}
			authority, err := authenticateRegisterProof(init, challenge, relay, proof, now)
			if err != nil {
				t.Fatal(err)
			}
			upload, err := ParseDescriptorUpload(testB64(t, test.DescriptorUploadB64))
			if err != nil {
				t.Fatal(err)
			}
			authorized, err := AuthorizeDescriptorUpload(init, authority, upload)
			if test.RelayUploadAuthorized {
				if err != nil {
					t.Fatalf("sender-authorized upload rejected: %v", err)
				}
				if object, ok := authorized.ObjectFor(init); !ok || !bytes.Equal(object, upload.Object) {
					t.Fatal("authorized upload did not retain the exact bytes")
				}
			} else if !errors.Is(err, ErrProof) {
				t.Fatalf("unauthorized upload error = %v", err)
			}
			_, err = catalogflow.OpenDescriptor(
				upload.Object, init.PKHash[:], init.ShareID[:], testB64(t, vector.DescriptorKeyB64),
			)
			if (err == nil) != test.ReceiverObjectValid {
				t.Fatalf("receiver object validity = %v, want %v: %v", err == nil, test.ReceiverObjectValid, err)
			}
		})
	}
}

func TestDescriptorUploadAuthorizationBoundsAndOwnsOpaqueBytes(t *testing.T) {
	for _, test := range []struct {
		name   string
		length int
		valid  bool
	}{
		{"empty", 0, false},
		{"single-byte", 1, true},
		{"maximum", MaxDescriptorBytes, true},
		{"oversized", MaxDescriptorBytes + 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			init, challenge, relay, privateKey := frozenRegistration(t)
			object := bytes.Repeat([]byte{0xff}, test.length)
			init.DescriptorDigest = sha256.Sum256(object)
			proof, err := NewRegisterProof(init, challenge, relay, privateKey)
			if err != nil {
				t.Fatal(err)
			}
			authority, err := authenticateRegisterProof(
				init, challenge, relay, proof, time.Unix(int64(challenge.ExpiresAtUnixSeconds)-1, 0),
			)
			if err != nil {
				t.Fatal(err)
			}
			authorized, err := AuthorizeDescriptorUpload(init, authority, DescriptorUpload{Object: object})
			if !test.valid {
				if !errors.Is(err, ErrMalformed) {
					t.Fatalf("out-of-bounds upload error = %v", err)
				}
				if _, ok := authorized.ObjectFor(init); ok {
					t.Fatal("rejected upload released bytes")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			object[0] ^= 1
			first, ok := authorized.ObjectFor(init)
			if !ok || sha256.Sum256(first) != init.DescriptorDigest {
				t.Fatal("caller mutation changed authorized upload bytes")
			}
			first[0] ^= 1
			second, ok := authorized.ObjectFor(init)
			if !ok || sha256.Sum256(second) != init.DescriptorDigest {
				t.Fatal("published bytes alias the authorized upload")
			}
		})
	}
}
