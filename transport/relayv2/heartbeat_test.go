package relayv2

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

var testHeartbeat = HeartbeatConfig{Interval: time.Second, Timeout: 3 * time.Second}

func startHeartbeatLink(t *testing.T, socket *scriptedSocket) *link {
	t.Helper()
	link := newLinkWithLifecycleStream(context.Background(), socket, false, 64)
	link.heartbeat = testHeartbeat
	link.start()
	return link
}

func takeProbe(t *testing.T, socket *scriptedSocket) uint64 {
	t.Helper()
	probe, err := v2.ParseConnectionProbe(assertWriteMagic(t, socket, v2.ConnectionProbeMagic))
	if err != nil {
		t.Fatal(err)
	}
	return probe.Nonce
}

func acknowledgeProbe(socket *scriptedSocket, nonce uint64) {
	ack, _ := v2.ConnectionProbeAck{Nonce: nonce}.MarshalBinary()
	socket.respond(ack)
}

func TestHeartbeatProbesOnCadenceAndTracesAcknowledgements(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		socket := newScriptedSocket()
		link := startHeartbeatLink(t, socket)
		for round := uint64(1); round <= 2; round++ {
			time.Sleep(testHeartbeat.Interval)
			if nonce := takeProbe(t, socket); nonce != round {
				t.Fatalf("probe nonce = %d, want %d", nonce, round)
			}
			acknowledgeProbe(socket, round)
			probe, ack := <-link.lifecycleTrace(), <-link.lifecycleTrace()
			if ValidateLifecycleTrace(probe) != LifecycleContractValid || ValidateLifecycleTrace(ack) != LifecycleContractValid {
				t.Fatal("heartbeat producer violated observation contract")
			}
			if probe.Stage != LifecycleHeartbeatProbe || ack.Stage != LifecycleHeartbeatAcknowledged ||
				ack.HeartbeatRound != round || ack.OperationID != probe.OperationID {
				t.Fatalf("probe=%+v acknowledgement=%+v", probe, ack)
			}
		}
		select {
		case <-link.done:
			t.Fatal("healthy idle link closed")
		default:
		}
		link.stop(nil)
		synctest.Wait()
	})
}

func TestHeartbeatKeepsProbingWhileContentOutlivesAcknowledgements(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		socket := newScriptedSocket()
		link := startHeartbeatLink(t, socket)
		retired, _ := v2.SessionRetired{RelaySessionID: relaySessionID(9)}.MarshalBinary()
		const rounds = 10
		for round := uint64(1); round <= rounds; round++ {
			time.Sleep(testHeartbeat.Interval)
			// Acknowledgements are queued behind content far beyond the timeout,
			// yet the relay keeps delivering accepted frames.
			socket.respond(retired)
			if nonce := takeProbe(t, socket); nonce != round {
				t.Fatalf("probe nonce = %d, want %d", nonce, round)
			}
		}
		synctest.Wait()
		if link.Err() != nil {
			t.Fatal("delayed acknowledgement closed a connection that kept delivering", link.Err())
		}
		acknowledgeProbe(socket, rounds)
		synctest.Wait()
		link.stop(nil)
		var acknowledged []uint64
		for event := range link.lifecycleTrace() {
			if event.Stage == LifecycleHeartbeatAcknowledged {
				acknowledged = append(acknowledged, event.HeartbeatRound)
			}
		}
		if len(acknowledged) != 1 || acknowledged[0] != rounds {
			t.Fatalf("coalesced acknowledgement traces = %v", acknowledged)
		}
	})
}

func TestHeartbeatRetiresSilentLink(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		link := startHeartbeatLink(t, newScriptedSocket())
		time.Sleep(testHeartbeat.Timeout)
		synctest.Wait()
		select {
		case <-link.done:
		default:
			t.Fatal("silent connection remains open")
		}
		if !errors.Is(link.Err(), ErrHeartbeat) {
			t.Fatal(link.Err())
		}
		seen := false
		for event := range link.lifecycleTrace() {
			if event.Stage == LifecycleHeartbeatFailed {
				if ValidateLifecycleTrace(event) != LifecycleContractValid {
					t.Fatal("failure rejected by producer contract")
				}
				seen = event.HeartbeatRound == 2 && event.Timeout == testHeartbeat.Timeout &&
					event.Wait == testHeartbeat.Timeout && event.Cause == LifecycleCauseDeadline
			}
		}
		if !seen {
			t.Fatal("missing correlated heartbeat failure")
		}
	})
}

func TestHeartbeatRejectsAcknowledgementOfUnsentProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		socket := newScriptedSocket()
		link := startHeartbeatLink(t, socket)
		acknowledgeProbe(socket, 1)
		synctest.Wait()
		if !errors.Is(link.Err(), ErrProtocol) {
			t.Fatal("acknowledgement of a probe never sent was accepted", link.Err())
		}
	})
}

func TestProbeLedgerAcceptsOnlyNewSentNonces(t *testing.T) {
	var ledger probeLedger
	now := time.Now()
	if _, _, err := ledger.acknowledge(1); !errors.Is(err, ErrProtocol) {
		t.Fatal("acknowledged before any probe")
	}
	first, _ := ledger.schedule(10, now)
	if _, scheduled := ledger.schedule(11, now); scheduled {
		t.Fatal("queued a second probe behind an unsent one")
	}
	if _, _, err := ledger.acknowledge(first); !errors.Is(err, ErrProtocol) {
		t.Fatal("acknowledged a probe the writer never took")
	}
	ledger.takeUnsent()
	for range maxTrackedProbes + 2 {
		ledger.schedule(12, now)
		ledger.takeUnsent()
	}
	newest := ledger.newestRound()
	record, correlated, err := ledger.acknowledge(newest - 1)
	if err != nil || !correlated || record.round != newest-1 {
		t.Fatalf("record=%+v correlated=%v err=%v", record, correlated, err)
	}
	if len(ledger.tracked) != 1 {
		t.Fatalf("coalesced acknowledgement kept %d older records", len(ledger.tracked))
	}
	if _, _, err := ledger.acknowledge(newest - 1); !errors.Is(err, ErrProtocol) {
		t.Fatal("repeated acknowledgement accepted")
	}
	if _, correlated, err := ledger.acknowledge(newest); err != nil || !correlated {
		t.Fatal("newest acknowledgement rejected", err)
	}
}

func TestHeartbeatConfigRequiresTimeoutBeyondCadence(t *testing.T) {
	if config, err := (HeartbeatConfig{}).Normalize(); err != nil ||
		config.Interval != v2.ConnectionProbeInterval || config.Timeout != v2.ConnectionSilenceTimeout {
		t.Fatalf("defaults = %+v, %v", config, err)
	}
	for _, config := range []HeartbeatConfig{
		{Interval: time.Second, Timeout: time.Second},
		{Interval: -time.Second},
	} {
		if _, err := config.Normalize(); !errors.Is(err, ErrHeartbeatConfig) {
			t.Fatalf("accepted %+v", config)
		}
	}
}

func TestHeartbeatLifecycleContractRejectsMalformedEvidence(t *testing.T) {
	valid := LifecycleTrace{LinkID: 1, OperationID: 1, Stage: LifecycleHeartbeatProbe,
		RetirementSource: LifecycleRetirementNone, Cause: LifecycleCauseNone, DrainCause: LifecycleCauseNone,
		HeartbeatRound: 1, Timeout: time.Second}
	for _, mutate := range []func(*LifecycleTrace){
		func(e *LifecycleTrace) { e.LinkID = 0 },
		func(e *LifecycleTrace) { e.OperationID = 0 },
		func(e *LifecycleTrace) { e.HeartbeatRound = 0 },
		func(e *LifecycleTrace) { e.RelaySessionID[0] = 1 },
		func(e *LifecycleTrace) { e.Timeout = 0 },
		func(e *LifecycleTrace) { e.Wait = -1 },
		func(e *LifecycleTrace) { e.Wait = 1 },
		func(e *LifecycleTrace) { e.Terminal = true },
		func(e *LifecycleTrace) { e.Cause = LifecycleCauseTransport },
		func(e *LifecycleTrace) { e.Stage = LifecycleHeartbeatFailed },
		func(e *LifecycleTrace) { e.Stage = LifecycleLinkClosed },
	} {
		event := valid
		mutate(&event)
		if ValidateLifecycleTrace(event) == LifecycleContractValid {
			t.Fatalf("accepted %+v", event)
		}
	}
}
