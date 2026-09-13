package runtrace

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/transport/relayv2"
)

func TestRecoveryAndAvailabilityPreserveLifecycleDecisionContext(t *testing.T) {
	authority, _ := clievent.NewRelayAuthority(clievent.RelayWSS, "relay.example", 443)
	identity, _ := clievent.NewSharingInstanceID([]byte("share-instance-1"))
	session, _ := clievent.NewProtocolSessionID([]byte("relay-session-01"))
	event, err := clievent.NewRelayRecoveryObservation(clievent.CommandShare, authority, 8, clievent.RelayRecoveryWaiting, clievent.Failure{}, clievent.RelayRecoveryDetails{ShareInstance: identity, Generation: 7, Slow: true, Resume: true, NextDelay: 30 * time.Second, ProtocolSessionID: session})
	if err != nil {
		t.Fatal(err)
	}
	record, err := encodeV4(testRunIdentity(0x10), entryMetadata{sequence: 1, time: time.Now()}, event)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(record)
	for _, expected := range []string{`"connection_generation":"7"`, `"slow_wait":true`, `"resume_registration":true`, `"next_delay_ms":"30000"`, `"share_instance":`, `"protocol_session_id":`} {
		if !strings.Contains(string(encoded), expected) {
			t.Fatalf("trace lost %s: %s", expected, encoded)
		}
	}
	availability, _ := clievent.NewRelayAvailability(0, 2, 1, true)
	record, err = encodeV4(testRunIdentity(0x10), entryMetadata{sequence: 2, time: time.Now()}, availability)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(record)
	if !strings.Contains(string(encoded), `"available":0,"total":2,"ever_ready":true,"terminal":1`) {
		t.Fatalf("availability=%s", encoded)
	}
}

func TestNativeHeartbeatTraceReachesCLIProjectionAndEncoding(t *testing.T) {
	for _, stage := range []relayv2.LifecycleStage{relayv2.LifecycleHeartbeatProbe, relayv2.LifecycleHeartbeatAcknowledged, relayv2.LifecycleHeartbeatFailed} {
		wait, cause := time.Second, relayv2.LifecycleCauseNone
		expectedWait := `"wait_ms":"1000"`
		if stage == relayv2.LifecycleHeartbeatProbe {
			wait = 0
			expectedWait = `"wait_ms":"0"`
		}
		if stage == relayv2.LifecycleHeartbeatFailed {
			cause = relayv2.LifecycleCauseTransport
		}
		event, err := commandprojection.ProjectRelayLifecycle(clievent.CommandShare, relayv2.LifecycleTrace{
			LinkID: 11, OperationID: 12, Stage: stage,
			RetirementSource: relayv2.LifecycleRetirementNone, Cause: cause, DrainCause: relayv2.LifecycleCauseNone,
			HeartbeatRound: 3, Wait: wait, Timeout: 10 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		record, err := encodeV4(testRunIdentity(0x10), entryMetadata{sequence: 1, time: time.Now()}, event)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(record)
		for _, expected := range []string{`"heartbeat_round":"3"`, expectedWait, `"timeout_ms":"10000"`, `"link_id":"11"`} {
			if !strings.Contains(string(encoded), expected) {
				t.Fatalf("heartbeat lost %s: %s", expected, encoded)
			}
		}
	}
}
