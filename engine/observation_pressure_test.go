package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine/internal/receive"
	"github.com/windshare/windshare/engine/internal/task"
	appobs "github.com/windshare/windshare/engine/observation"
)

const pressureObservations = 4096

func pressureEvent(kind string) task.Event {
	if kind == "progress" {
		return receive.ProgressObserved{
			Operation: receivecontract.OperationID{1}, Job: transfer.TransferJobID{2},
			Value: transfer.ReceiveProgressSnapshot{
				DiscoveredFiles: 1, DiscoveredBytes: 1 << 20, VerifiedBytes: 1 << 19,
				NewlyVerifiedBytes: 1 << 19, Discovery: transfer.DiscoveryComplete, CountersExact: true,
			},
		}
	}
	source := sessionruntime.ProtocolObservationContext{
		ObservedAt: time.Unix(1, 123),
		Correlation: sessionruntime.ProtocolObservationCorrelation{
			Role: protocolsession.RoleReceiver, ProtocolSessionID: protocolsession.ProtocolSessionID{1},
			OperationID: protocolsession.OperationID{2}, RequestKind: protocolsession.MessageOpenRevisions,
		},
	}
	return receive.ProtocolObserved{Value: sessionruntime.NewProtocolOperationObservation(source,
		sessionruntime.ProtocolOperationObservation{
			Stage: sessionruntime.ProtocolOperationReceiverEnded, Cause: sessionruntime.ProtocolOperationCauseNone,
		})}
}

// Exercise the actual application publisher, not just the lower queue. A slow
// observer must not acquire authority over task cancellation or final settlement.
func TestProjectedObservationPressurePreservesCancellationAndSettlement(t *testing.T) {
	const queueCapacity = 8
	application := newTestEngine(t, Config{ObservationCapacity: queueCapacity})
	produced := make(chan struct{})
	current, err := startWorkflow(application, t.Context(), appobs.WorkflowReceive,
		func(ctx context.Context, control task.Control) task.Completion[int] {
			fact := pressureEvent("protocol")
			for range pressureObservations {
				control.Emit(fact)
			}
			close(produced)
			<-ctx.Done()
			return task.Completion[int]{
				Value:      pressureObservations,
				Settlement: task.Settlement{Outcome: task.OutcomeCancelled, Err: context.Cause(ctx)},
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	<-produced
	current.Cancel()
	wait, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result, err := current.Wait(wait)
	if err != nil || result.Outcome != OutcomeCancelled || result.Value != pressureObservations ||
		!errors.Is(result.Err, task.ErrCancelled) || result.Failure.Code() != appobs.FailureCanceled {
		t.Fatalf("observer changed cancellation: result=%+v err=%v", result, err)
	}
	if result.Observations.Enqueued != queueCapacity || result.Observations.CapacityDropped == 0 {
		t.Fatalf("queue did not remain bounded: %+v", result.Observations)
	}
	retained := 0
	for envelope := range current.Observations() {
		retained++
		switch event := envelope.Event.(type) {
		case appobs.Event:
			if err := event.Validate(); err != nil {
				t.Fatalf("invalid application fact: %T: %v", event, err)
			}
			if _, rejected := event.(appobs.ObserverLossObserved); rejected {
				t.Fatal("valid protocol fixture was rejected")
			}
		default:
			t.Fatalf("native observation escaped: %T", envelope.Event)
		}
	}
	if retained != queueCapacity {
		t.Fatalf("retained=%d", retained)
	}
}

// Saturated mode measures producer cost even when no client wants more facts.
// Drained mode includes the application's queue handoff, but excludes rendering.
func BenchmarkEngineObservationPressure(b *testing.B) {
	for _, kind := range []string{"protocol", "progress"} {
		for _, mode := range []string{"drained", "saturated"} {
			b.Run(fmt.Sprintf("%s/%s", kind, mode), func(b *testing.B) {
				application, err := New(Config{ObservationCapacity: 64})
				if err != nil {
					b.Fatal(err)
				}
				defer func() {
					if err := application.Close(context.Background()); err != nil {
						b.Fatal(err)
					}
				}()
				fact := pressureEvent(kind)
				var dropped uint64
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					current, err := startWorkflow(application, context.Background(), appobs.WorkflowReceive,
						func(_ context.Context, control task.Control) task.Completion[int] {
							for range pressureObservations {
								control.Emit(fact)
							}
							return task.Completion[int]{Value: pressureObservations, Settlement: task.Settlement{Outcome: task.OutcomeSuccess}}
						})
					if err != nil {
						b.Fatal(err)
					}
					drained := make(chan struct{})
					if mode == "drained" {
						go func() {
							for range current.Observations() {
							}
							close(drained)
						}()
					}
					result, err := current.Wait(context.Background())
					if err != nil || result.Outcome != OutcomeSuccess {
						b.Fatalf("task did not finish: %+v, %v", result, err)
					}
					dropped += result.Observations.CapacityDropped
					if mode == "drained" {
						<-drained
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(pressureObservations), "events/op")
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/pressureObservations, "ns/event")
				b.ReportMetric(float64(dropped)/float64(b.N), "dropped/op")
			})
		}
	}
}
