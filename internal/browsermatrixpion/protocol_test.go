package browsermatrixpion

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRemotePeerBindingIsStableAndRejectsAmbiguousEndpoints(t *testing.T) {
	fixture := testExternalFixture("scheduled-public-stun")
	first, err := RemotePeerBindingSHA256FromFixture(fixture)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RemotePeerBindingSHA256FromFixture(fixture)
	if err != nil || first != second || len(first) != 64 {
		t.Fatalf("binding is not stable: first=%q second=%q err=%v", first, second, err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*ExternalFixture)
	}{
		{"bad instance", func(value *ExternalFixture) { value.RemoteServiceInstanceID = "Remote_A" }},
		{"noncanonical address", func(value *ExternalFixture) { value.RemotePeerPublicIP = "203.000.113.010" }},
		{"ipv6", func(value *ExternalFixture) { value.RemotePeerPublicIP = "2001:db8::1" }},
		{"zero port", func(value *ExternalFixture) { value.RemotePeerUDPPortMin = 0 }},
		{"reversed range", func(value *ExternalFixture) { value.RemotePeerUDPPortMax = 40000 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := fixture
			test.mutate(&invalid)
			if _, err := RemotePeerBindingSHA256FromFixture(invalid); err == nil {
				t.Fatal("invalid endpoint accepted")
			}
		})
	}
}

func TestAttemptWireBindingOrderIsCanonical(t *testing.T) {
	binding := testAttemptBinding()
	request := testCreateAttemptRequest(binding, "request-00000001", 1_000)
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	authorityDocument, err := CanonicalAttemptRequestAuthorityDocument(request.RequestAuthority)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"protocolVersion":"` + ProtocolVersion + `","requestAuthority":` +
		strings.TrimSpace(string(authorityDocument)) + `,"leaseMillis":1000}`
	if string(encoded) != want {
		t.Fatalf("create binding field order changed:\n got %s\nwant %s", encoded, want)
	}
}

func TestRequestValidationIsExact(t *testing.T) {
	binding := testAttemptBinding()
	validProbe := ProbeRequest{
		ProtocolVersion: ProtocolVersion, AttemptBinding: binding,
		Nonce: strings.Repeat("n", 16), STUNURI: "stun:stun.example:3478",
	}
	if err := validateProbeRequest(validProbe); err != nil {
		t.Fatal(err)
	}
	invalidProbe := validProbe
	invalidProbe.ProtocolVersion = "v0"
	if validateProbeRequest(invalidProbe) == nil {
		t.Fatal("wrong protocol accepted")
	}
	validCreate := testCreateAttemptRequest(binding, strings.Repeat("r", 16), 500)
	if lease, err := validateCreateRequest(validCreate, time.Second); err != nil || lease != 500*time.Millisecond {
		t.Fatalf("valid lease rejected: %v, %v", lease, err)
	}
	validCreate.LeaseMillis = 1001
	if _, err := validateCreateRequest(validCreate, time.Second); err == nil {
		t.Fatal("oversized lease accepted")
	}
	authority := testAttemptAuthority(strings.Repeat("i", 16), strings.Repeat("c", 43))
	validOffer := OfferRequest{
		ProtocolVersion: ProtocolVersion, AttemptAuthority: authority,
		Type: "offer", SDP: "v=0\r\n",
	}
	if err := validateOfferRequest(validOffer, authority); err != nil {
		t.Fatal(err)
	}
	validOffer.SDP += "\x00"
	if validateOfferRequest(validOffer, authority) == nil {
		t.Fatal("NUL-bearing SDP accepted")
	}
}

func TestDecodeRequestRejectsUnknownAndTrailingData(t *testing.T) {
	for _, body := range []string{
		`{"protocolVersion":"x","unknown":true}`,
		`{"protocolVersion":"x"}{}`,
		strings.Repeat("x", maximumBodyBytes+1),
	} {
		request := httptest.NewRequest("POST", "/", bytes.NewBufferString(body))
		response := httptest.NewRecorder()
		var destination struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := decodeRequest(response, request, &destination); err == nil {
			t.Fatalf("invalid body accepted: %.40q", body)
		}
	}
}
