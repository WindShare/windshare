package v2

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

type v2VectorFile struct {
	Cases []json.RawMessage `json:"cases"`
}

type v2IdentityVector struct {
	Name               string `json:"name"`
	SenderPublicKeyB64 string `json:"senderPublicKeyB64"`
	PKHashB64          string `json:"pkHashB64"`
	ShareInstanceB64   string `json:"shareInstanceB64"`
	ShareIDRawB64      string `json:"shareIdRawB64"`
}

type v2RegistrationVector struct {
	Name                    string `json:"name"`
	DescriptorDigestB64     string `json:"descriptorDigestB64"`
	ResumeTokenB64          string `json:"resumeTokenB64"`
	ResumeTokenHashB64      string `json:"resumeTokenHashB64"`
	RelayIdentityB64        string `json:"relayIdentityB64"`
	ChallengeIDB64          string `json:"challengeIdB64"`
	ChallengeNonceB64       string `json:"challengeNonceB64"`
	ExpiresAt               string `json:"expiresAt"`
	SignatureB64            string `json:"signatureB64"`
	RegisterInitB64         string `json:"registerInitB64"`
	RegisterChallengeB64    string `json:"registerChallengeB64"`
	RegisterProofB64        string `json:"registerProofB64"`
	ResumeChallengeIDB64    string `json:"resumeChallengeIdB64"`
	ResumeChallengeNonceB64 string `json:"resumeChallengeNonceB64"`
	ResumeExpiresAt         string `json:"resumeExpiresAt"`
	ResumeInitB64           string `json:"resumeInitB64"`
	ResumeChallengeB64      string `json:"resumeChallengeB64"`
	ResumeSignatureB64      string `json:"resumeSignatureB64"`
	ResumeProofB64          string `json:"resumeProofB64"`
	ResumeCredentialB64     string `json:"resumeCredentialB64"`
	StopIDB64               string `json:"stopIdB64"`
	StopChallengeIDB64      string `json:"stopChallengeIdB64"`
	StopChallengeNonceB64   string `json:"stopChallengeNonceB64"`
	StopExpiresAt           string `json:"stopExpiresAt"`
	StopInitB64             string `json:"stopInitB64"`
	StopChallengeB64        string `json:"stopChallengeB64"`
	StopSignatureB64        string `json:"stopSignatureB64"`
	StopProofB64            string `json:"stopProofB64"`
	StoppedB64              string `json:"stoppedB64"`
	OpaqueRelaySessionIDB64 string `json:"opaqueRelaySessionIdB64"`
	OpaqueCiphertextB64     string `json:"opaqueCiphertextB64"`
	OpaqueRouteB64          string `json:"opaqueRouteB64"`
	StoppedErrorB64         string `json:"stoppedErrorB64"`
}

func loadV2VectorCase(t *testing.T, fileName, name string, destination any) {
	t.Helper()
	encoded, err := os.ReadFile(filepath.Join("..", "..", "..", "core", "testvectors", fileName))
	if err != nil {
		t.Fatal(err)
	}
	var file v2VectorFile
	if err := json.Unmarshal(encoded, &file); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range file.Cases {
		var identity struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(candidate, &identity); err != nil {
			t.Fatal(err)
		}
		if identity.Name == name {
			if err := json.Unmarshal(candidate, destination); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("%s does not contain vector %q", fileName, name)
}

func TestRuntimeReconstructsGeneratedRegistrationResumeStopAndOpaqueVectors(t *testing.T) {
	var identity v2IdentityVector
	loadV2VectorCase(t, "v2-identity.json", "suite-02-link-and-keys", &identity)
	var vector v2RegistrationVector
	loadV2VectorCase(t, "v2-session.json", "fresh-relay-registration-proof", &vector)

	init := RegisterInit{Mode: RegistrationFresh}
	copy(init.ShareID[:], testB64(t, identity.ShareIDRawB64))
	copy(init.ShareInstance[:], testB64(t, identity.ShareInstanceB64))
	copy(init.PKHash[:], testB64(t, identity.PKHashB64))
	copy(init.DescriptorDigest[:], testB64(t, vector.DescriptorDigestB64))
	copy(init.ResumeTokenHash[:], testB64(t, vector.ResumeTokenHashB64))
	challenge := Challenge{Purpose: ChallengeRegister, ExpiresAtUnixSeconds: parseVectorUint64(t, vector.ExpiresAt)}
	copy(challenge.ID[:], testB64(t, vector.ChallengeIDB64))
	copy(challenge.Nonce[:], testB64(t, vector.ChallengeNonceB64))
	var relayIdentity RelayIdentity
	copy(relayIdentity[:], testB64(t, vector.RelayIdentityB64))
	privateKey := ed25519.NewKeyFromSeed(testSequence(0x20, ed25519.SeedSize))

	assertBinaryVector(t, "REGISTER_INIT", init.MarshalBinary, vector.RegisterInitB64)
	assertBinaryVector(t, "REGISTER_CHALLENGE", challenge.MarshalBinary, vector.RegisterChallengeB64)
	proof, err := NewRegisterProof(init, challenge, relayIdentity, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	assertBinaryVector(t, "REGISTER_PROOF", proof.MarshalBinary, vector.RegisterProofB64)

	resume := init
	resume.Mode = RegistrationResume
	resumeChallenge := Challenge{Purpose: ChallengeResume, ExpiresAtUnixSeconds: parseVectorUint64(t, vector.ResumeExpiresAt)}
	copy(resumeChallenge.ID[:], testB64(t, vector.ResumeChallengeIDB64))
	copy(resumeChallenge.Nonce[:], testB64(t, vector.ResumeChallengeNonceB64))
	assertBinaryVector(t, "RESUME_INIT", resume.MarshalBinary, vector.ResumeInitB64)
	assertBinaryVector(t, "RESUME_CHALLENGE", resumeChallenge.MarshalBinary, vector.ResumeChallengeB64)
	resumeProof, err := NewRegisterProof(resume, resumeChallenge, relayIdentity, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	assertBinaryVector(t, "RESUME_PROOF", resumeProof.MarshalBinary, vector.ResumeProofB64)
	var credential ResumeCredential
	copy(credential.Token[:], testB64(t, vector.ResumeTokenB64))
	assertBinaryVector(t, "RESUME_CREDENTIAL", credential.MarshalBinary, vector.ResumeCredentialB64)

	stop := StopInit{ShareID: init.ShareID, ShareInstance: init.ShareInstance, PKHash: init.PKHash, RelayIdentity: relayIdentity}
	copy(stop.StopID[:], testB64(t, vector.StopIDB64))
	stopChallenge := Challenge{Purpose: ChallengeStop, ExpiresAtUnixSeconds: parseVectorUint64(t, vector.StopExpiresAt)}
	copy(stopChallenge.ID[:], testB64(t, vector.StopChallengeIDB64))
	copy(stopChallenge.Nonce[:], testB64(t, vector.StopChallengeNonceB64))
	assertBinaryVector(t, "STOP_INIT", stop.MarshalBinary, vector.StopInitB64)
	assertBinaryVector(t, "STOP_CHALLENGE", stopChallenge.MarshalBinary, vector.StopChallengeB64)
	stopProof, err := NewStopProof(stop, stopChallenge, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	assertBinaryVector(t, "STOP_PROOF", stopProof.MarshalBinary, vector.StopProofB64)
	assertBinaryVector(t, "STOPPED", (Stopped{StopID: stop.StopID}).MarshalBinary, vector.StoppedB64)

	var session RelaySessionID
	copy(session[:], testB64(t, vector.OpaqueRelaySessionIDB64))
	opaque := OpaqueRoute{RelaySessionID: session, Ciphertext: testB64(t, vector.OpaqueCiphertextB64)}
	assertBinaryVector(t, "OPAQUE_ROUTE", opaque.MarshalBinary, vector.OpaqueRouteB64)
	assertBinaryVector(t, "STOPPED_ERROR", (ErrorFrame{Code: ErrorStopped}).MarshalBinary, vector.StoppedErrorB64)
}

func assertBinaryVector(t *testing.T, label string, marshal func() ([]byte, error), encoded string) {
	t.Helper()
	got, err := marshal()
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	if !bytes.Equal(got, testB64(t, encoded)) {
		t.Fatalf("%s diverged from generated vector", label)
	}
}

func parseVectorUint64(t *testing.T, value string) uint64 {
	t.Helper()
	var result uint64
	if _, err := fmt.Sscan(value, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
