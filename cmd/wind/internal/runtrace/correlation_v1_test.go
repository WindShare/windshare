package runtrace

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

func TestCorrelationV1ProjectsTypedIdentitiesAsUnpaddedBase64URL(t *testing.T) {
	sessionBytes := correlationSequence(0x00)
	operationBytes := correlationSequence(0xf0)
	pathBytes := correlationSequence(0x20)
	attemptBytes := correlationSequence(0xe0)

	session := mustCorrelationProtocolSessionID(t, sessionBytes)
	operation := mustCorrelationProtocolOperationID(t, operationBytes)
	path := mustCorrelationPeerPathID(t, pathBytes)
	attempt := mustCorrelationPeerAttemptID(t, attemptBytes)
	lane := &LaneCorrelation{ID: 7, Epoch: 2}

	// Construction owns a byte copy; later mutation of caller storage cannot
	// rewrite diagnostic identity.
	sessionBytes[0] = 0xff

	projected, err := ProjectCorrelationV1(CorrelationInput{
		ProtocolSessionID:   session,
		ProtocolOperationID: operation,
		PeerPathID:          path,
		PeerAttemptID:       attempt,
		Lane:                lane,
	})
	if err != nil {
		t.Fatal(err)
	}

	expectedSession := base64.RawURLEncoding.EncodeToString(correlationSequence(0x00))
	if projected == nil || projected.ProtocolSessionID != expectedSession {
		t.Fatalf("protocol session ID = %#v, want %q", projected, expectedSession)
	}
	for name, encoded := range map[string]string{
		"protocol session":   projected.ProtocolSessionID,
		"protocol operation": projected.ProtocolOperationID,
		"peer path":          projected.PeerPathID,
		"peer attempt":       projected.PeerAttemptID,
	} {
		if len(encoded) != 22 || strings.Contains(encoded, "=") {
			t.Fatalf("%s identity = %q, want 22-character unpadded base64url", name, encoded)
		}
	}
	if !strings.ContainsAny(projected.ProtocolOperationID+projected.PeerAttemptID, "-_") {
		t.Fatal("test identities do not exercise the base64url alphabet")
	}

	encoded, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"protocol_session_id":"` + projected.ProtocolSessionID +
		`","protocol_operation_id":"` + projected.ProtocolOperationID +
		`","peer_path_id":"` + projected.PeerPathID +
		`","peer_attempt_id":"` + projected.PeerAttemptID +
		`","lane_id":7,"lane_epoch":2}`
	if string(encoded) != want {
		t.Fatalf("correlation JSON = %s, want %s", encoded, want)
	}
}

func TestCorrelationV1RejectsOrphanDependentIdentities(t *testing.T) {
	operation := mustCorrelationProtocolOperationID(t, correlationSequence(0x40))
	attempt := mustCorrelationPeerAttemptID(t, correlationSequence(0x50))

	for name, input := range map[string]CorrelationInput{
		"operation without session": {ProtocolOperationID: operation},
		"attempt without path":      {PeerAttemptID: attempt},
	} {
		t.Run(name, func(t *testing.T) {
			projected, err := ProjectCorrelationV1(input)
			if !errors.Is(err, ErrInvalidCorrelation) || projected != nil {
				t.Fatalf("ProjectCorrelationV1() = (%+v, %v), want nil ErrInvalidCorrelation", projected, err)
			}
		})
	}
}

func TestCorrelationV1OmitsEmptyInput(t *testing.T) {
	projected, err := ProjectCorrelationV1(CorrelationInput{})
	if err != nil {
		t.Fatal(err)
	}
	if projected != nil {
		t.Fatalf("empty correlation = %+v, want nil", projected)
	}
}

func TestCorrelationV1RetainsLaneUint32Boundaries(t *testing.T) {
	for _, testCase := range []struct {
		name string
		lane LaneCorrelation
	}{
		{name: "zero", lane: LaneCorrelation{ID: 0, Epoch: 0}},
		{name: "maximum", lane: LaneCorrelation{ID: math.MaxUint32, Epoch: math.MaxUint32}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			projected, err := ProjectCorrelationV1(CorrelationInput{Lane: &testCase.lane})
			if err != nil {
				t.Fatal(err)
			}
			if projected == nil || projected.LaneID == nil || projected.LaneEpoch == nil ||
				*projected.LaneID != testCase.lane.ID ||
				*projected.LaneEpoch != testCase.lane.Epoch {
				t.Fatalf(
					"lane correlation = %+v, want id=%d epoch=%d",
					projected,
					testCase.lane.ID,
					testCase.lane.Epoch,
				)
			}
			encoded, err := json.Marshal(projected)
			if err != nil {
				t.Fatal(err)
			}
			if testCase.lane.ID == 0 &&
				!strings.Contains(string(encoded), `"lane_id":0`) {
				t.Fatalf("zero lane ID was omitted: %s", encoded)
			}
			if testCase.lane.Epoch == 0 &&
				!strings.Contains(string(encoded), `"lane_epoch":0`) {
				t.Fatalf("zero lane epoch was omitted: %s", encoded)
			}
		})
	}
}

func correlationSequence(start byte) []byte {
	raw := make([]byte, clievent.IdentityBytes)
	for index := range raw {
		raw[index] = start + byte(index)
	}
	return raw
}

func mustCorrelationProtocolSessionID(t *testing.T, raw []byte) clievent.ProtocolSessionID {
	t.Helper()
	value, err := clievent.NewProtocolSessionID(raw)
	return mustCorrelationValue(t, value, err)
}

func mustCorrelationProtocolOperationID(t *testing.T, raw []byte) clievent.ProtocolOperationID {
	t.Helper()
	value, err := clievent.NewProtocolOperationID(raw)
	return mustCorrelationValue(t, value, err)
}

func mustCorrelationPeerPathID(t *testing.T, raw []byte) clievent.PeerPathID {
	t.Helper()
	value, err := clievent.NewPeerPathID(raw)
	return mustCorrelationValue(t, value, err)
}

func mustCorrelationPeerAttemptID(t *testing.T, raw []byte) clievent.PeerAttemptID {
	t.Helper()
	value, err := clievent.NewPeerAttemptID(raw)
	return mustCorrelationValue(t, value, err)
}

func mustCorrelationValue[T any](t *testing.T, value T, err error) T {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return value
}
