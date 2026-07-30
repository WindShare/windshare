package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/internal/testicetopology"
)

type fixedTopologyResolver struct {
	resolution testicetopology.Resolution
	err        error
}

func (resolver fixedTopologyResolver) Resolve(
	context.Context,
	testicetopology.Profile,
) (testicetopology.Resolution, error) {
	return resolver.resolution, resolver.err
}

func TestLoadTopologyRuntimeBindsCanonicalSerializedLock(t *testing.T) {
	profilePath, resolutionPath, profile, resolution := topologyFixtures(t)
	profileDigest, err := profile.SHA256()
	if err != nil {
		t.Fatalf("profile SHA-256: %v", err)
	}
	resolutionDigest, err := resolution.SHA256(profile, profileDigest)
	if err != nil {
		t.Fatalf("resolution SHA-256: %v", err)
	}
	environment := map[string]string{
		topologyProfilePathEnv:      profilePath,
		topologyResolutionPathEnv:   resolutionPath,
		expectedProfileDigestEnv:    profileDigest,
		expectedResolutionDigestEnv: resolutionDigest,
	}
	runtime, err := loadTopologyRuntimeWith(
		context.Background(),
		func(name string) string { return environment[name] },
		fixedTopologyResolver{resolution: resolution},
	)
	if err != nil {
		t.Fatalf("load topology runtime: %v", err)
	}
	if runtime == nil || runtime.profileSHA256 != profileDigest || runtime.resolutionSHA256 != resolutionDigest {
		t.Fatalf("runtime lock = %#v", runtime)
	}
	if !json.Valid(runtime.public.Profile) || !json.Valid(runtime.public.Resolution) {
		t.Fatal("public topology lock is not JSON")
	}
}

func TestLoadTopologyRuntimeRejectsStaleSerializedResolution(t *testing.T) {
	profilePath, resolutionPath, _, resolution := topologyFixtures(t)
	current := resolution
	current.Interface.SelectedAddress = "192.0.2.11"
	current.Interface.EligibleAddresses = []testicetopology.EligibleAddress{{
		Address: "192.0.2.11", PrefixLength: 24,
	}}
	for index := range current.ProbeResults {
		current.ProbeResults[index].SourceAddress = current.Interface.SelectedAddress
	}
	_, err := loadTopologyRuntimeWith(
		context.Background(),
		func(name string) string {
			switch name {
			case topologyProfilePathEnv:
				return profilePath
			case topologyResolutionPathEnv:
				return resolutionPath
			default:
				return ""
			}
		},
		fixedTopologyResolver{resolution: current},
	)
	if err == nil || !strings.Contains(err.Error(), "does not match the current kernel route") {
		t.Fatalf("load stale resolution error = %v", err)
	}
}

func TestTopologySelectedPairValidationIsResolutionBound(t *testing.T) {
	_, _, profile, resolution := topologyFixtures(t)
	runtime := &topologyRuntime{profile: profile, resolution: resolution}
	pair := pionSelectedPairEvidence{
		Local: pionCandidateEvidence{
			CandidateType: "host", Protocol: "udp", Address: "192.0.2.10", Port: 40_001,
			AddressFamily: "ipv4",
		},
		Remote: pionCandidateEvidence{
			CandidateType: "prflx", Protocol: "udp", Address: "192.0.2.10", Port: 40_000,
			AddressFamily: "ipv4",
		},
	}
	if err := runtime.validateSelectedPair(pair); err != nil {
		t.Fatalf("validate direct pair: %v", err)
	}
	relay := pair
	relay.Remote.CandidateType = "relay"
	if err := runtime.validateSelectedPair(relay); err == nil {
		t.Fatal("relay selected pair was accepted")
	}
	wrongLocal := pair
	wrongLocal.Local.Address = "192.0.2.11"
	if err := runtime.validateSelectedPair(wrongLocal); err == nil {
		t.Fatal("non-selected Pion local address was accepted")
	}
}

func TestTopologyOfferRequiresCanonicalAttemptIdentity(t *testing.T) {
	attemptID := "BwcHBwcHBwcHBwcHBwcHBw"
	envelope, err := json.Marshal(topologyOfferRequest{
		AttemptID: attemptID,
		Offer:     pion.SessionDescription{Type: pion.SDPTypeOffer, SDP: "v=0\r\n"},
	})
	if err != nil {
		t.Fatalf("marshal offer: %v", err)
	}
	server := &interopServer{topology: &topologyRuntime{}}
	request := requestWithBody(envelope)
	offer, gotAttemptID, err := server.decodeOffer(request)
	if err != nil {
		t.Fatalf("decode topology offer: %v", err)
	}
	if offer.Type != pion.SDPTypeOffer || gotAttemptID != attemptID {
		t.Fatalf("decoded offer = %#v, attempt = %q", offer, gotAttemptID)
	}

	invalid := bytes.Replace(envelope, []byte(attemptID), []byte("AAAAAAAAAAAAAAAAAAAAAA"), 1)
	if _, _, err := server.decodeOffer(requestWithBody(invalid)); err == nil {
		t.Fatal("zero native attempt identity was accepted")
	}
	unknown := bytes.TrimSuffix(envelope, []byte("}"))
	unknown = append(unknown, []byte(`,"unexpected":true}`)...)
	if _, _, err := server.decodeOffer(requestWithBody(unknown)); err == nil {
		t.Fatal("unknown topology offer field was accepted")
	}
}

func topologyFixtures(
	t *testing.T,
) (string, string, testicetopology.Profile, testicetopology.Resolution) {
	t.Helper()
	root := filepath.Join("..", "..", "..", "..", "..")
	profilePath := filepath.Join(root, "testdata", "test-ice-topology", "pr-same-host-kernel-route-ipv4.json")
	resolutionPath := filepath.Join(root, "testdata", "test-ice-topology", "pr-same-host-kernel-route-ipv4-resolution.json")
	profile, err := testicetopology.Load(profilePath)
	if err != nil {
		t.Fatalf("load profile fixture: %v", err)
	}
	profileDigest, err := profile.SHA256()
	if err != nil {
		t.Fatalf("profile fixture digest: %v", err)
	}
	resolution, err := testicetopology.LoadResolution(resolutionPath, profile, profileDigest)
	if err != nil {
		t.Fatalf("load resolution fixture: %v", err)
	}
	return profilePath, resolutionPath, profile, resolution
}

func requestWithBody(body []byte) *http.Request {
	request, err := http.NewRequest(http.MethodPost, "/offer", bytes.NewReader(body))
	if err != nil {
		panic(err)
	}
	return request
}
