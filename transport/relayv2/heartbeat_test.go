package relayv2

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func (*scriptedSocket) Ping(context.Context) error             { return nil }
func (*registrationContractSocket) Ping(context.Context) error { return nil }

type silentHeartbeatSocket struct{ *scriptedSocket }

func (*silentHeartbeatSocket) Ping(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

func TestHeartbeatKeepsIdleLinkAndTracesAcknowledgements(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		link := newLinkWithLifecycleStream(context.Background(), newScriptedSocket(), false, 32)
		link.heartbeat = HeartbeatConfig{Interval: time.Second, Timeout: 3 * time.Second}
		link.start()
		for round := uint64(1); round <= 2; round++ {
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

func TestHeartbeatRetiresSilentLink(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		socket := &silentHeartbeatSocket{scriptedSocket: newScriptedSocket()}
		link := newLinkWithLifecycleStream(context.Background(), socket, false, 32)
		link.heartbeat = HeartbeatConfig{Interval: time.Second, Timeout: 3 * time.Second}
		link.start()
		synctest.Wait()
		time.Sleep(4 * time.Second)
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
				seen = event.HeartbeatRound == 1 && event.Timeout == 3*time.Second && event.Wait == 3*time.Second
			}
		}
		if !seen {
			t.Fatal("missing correlated heartbeat failure")
		}
	})
}
