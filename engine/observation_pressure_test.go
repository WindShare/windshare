package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine/internal/receive"
	"github.com/windshare/windshare/engine/internal/task"
	appobs "github.com/windshare/windshare/engine/observation"
)

const pressureObservations = 4096

func pressureProgress() task.Event {
	return receive.ProgressObserved{
		Operation: receivecontract.OperationID{1}, Job: transfer.TransferJobID{2},
		Value: transfer.ReceiveProgressSnapshot{DiscoveredFiles: 1, DiscoveredBytes: 1 << 20,
			VerifiedBytes: 1 << 19, NewlyVerifiedBytes: 1 << 19, Discovery: transfer.DiscoveryComplete, CountersExact: true},
	}
}

func TestDiagnosticPressurePreservesProductProgressCancellationAndSettlement(t *testing.T) {
	application := newTestEngine(t, Config{ObservationCapacity: 8})
	stream, _ := diagnostics.NewStream(1, nil)
	produced := make(chan struct{})
	current, err := startWorkflow(application, t.Context(), appobs.WorkflowReceive, stream.Sink(),
		func(ctx context.Context, control task.Control) task.Completion[int] {
			record := diagnostics.New("future.diagnostic.stage", diagnostics.Fields{"round": diagnostics.Uint(1)})
			for range pressureObservations {
				control.Diagnostics.Emit(record)
			}
			control.Emit(pressureProgress())
			close(produced)
			<-ctx.Done()
			return task.Completion[int]{Value: 42, Settlement: task.Settlement{Outcome: task.OutcomeCancelled, Err: context.Cause(ctx)}}
		})
	if err != nil {
		t.Fatal(err)
	}
	<-produced
	current.Cancel()
	wait, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result, err := current.Wait(wait)
	if err != nil || result.Value != 42 || result.Outcome != OutcomeCancelled || !errors.Is(result.Err, task.ErrCancelled) {
		t.Fatalf("cancellation: %+v %v", result, err)
	}
	if result.Observations.CapacityDropped != 0 {
		t.Fatalf("diagnostics displaced product facts: %+v", result.Observations)
	}
	progress, settled := false, false
	for envelope := range current.Observations() {
		switch event := envelope.Event.(type) {
		case appobs.TransferProgress:
			progress = true
		case appobs.EngineTaskObserved:
			settled = settled || event.Facts().Stage == appobs.EngineTaskSettled
		default:
			t.Fatalf("unexpected product event: %T", event)
		}
	}
	if !progress || !settled {
		t.Fatalf("progress=%t settled=%t", progress, settled)
	}
	cut := stream.Close()
	if cut.Enqueued != 1 || cut.CapacityDropped != pressureObservations-1 {
		t.Fatalf("trace capacity: %+v", cut)
	}
	record := <-stream.Records()
	if record.Scope().TaskID != string(current.ID()) || record.Scope().EngineID == "" || record.Scope().Workflow != "receive" {
		t.Fatalf("missing task correlation: %+v", record.Scope())
	}
}

func TestClosedDiagnosticStreamDoesNotAffectTasksAndScopesRemainDistinct(t *testing.T) {
	application := newTestEngine(t, Config{ObservationCapacity: 1})
	stream, _ := diagnostics.NewStream(4, nil)
	var tasks []*Task[int]
	for range 2 {
		current, err := startWorkflow(application, t.Context(), appobs.WorkflowShare, stream.Sink(), func(_ context.Context, c task.Control) task.Completion[int] {
			c.Diagnostics.Emit(diagnostics.New("source.started", nil))
			return task.Completion[int]{Value: 1, Settlement: task.Settlement{Outcome: task.OutcomeSuccess}}
		})
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, current)
	}
	for _, current := range tasks {
		first, err := current.Wait(t.Context())
		if err != nil || first.Outcome != OutcomeSuccess {
			t.Fatal(first, err)
		}
		second, err := current.Wait(t.Context())
		if err != nil || second.Value != first.Value || second.FinishedAt != first.FinishedAt {
			t.Fatal("retained result changed")
		}
	}
	stream.Close()
	seen := map[string]bool{}
	for record := range stream.Records() {
		seen[record.Scope().TaskID] = true
	}
	if len(seen) != 2 {
		t.Fatalf("task scopes merged: %v", seen)
	}
	current, err := startWorkflow(application, t.Context(), appobs.WorkflowShare, stream.Sink(), func(_ context.Context, c task.Control) task.Completion[int] {
		if c.Diagnostics.Emit(diagnostics.New("after.close", nil)) {
			t.Error("closed sink accepted event")
		}
		return task.Completion[int]{Settlement: task.Settlement{Outcome: task.OutcomeSuccess}}
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := current.Wait(t.Context()); err != nil || result.Outcome != OutcomeSuccess {
		t.Fatal(result, err)
	}
}

func BenchmarkEngineObservationPressure(b *testing.B) {
	for _, kind := range []string{"diagnostic", "progress"} {
		b.Run(kind, func(b *testing.B) {
			application, err := New(Config{ObservationCapacity: 64})
			if err != nil {
				b.Fatal(err)
			}
			defer application.Close(context.Background())
			stream, _ := diagnostics.NewStream(64, nil)
			defer stream.Close()
			record := diagnostics.New("source.stage", diagnostics.Fields{"count": diagnostics.Uint(1)})
			progress := pressureProgress()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				current, err := startWorkflow(application, context.Background(), appobs.WorkflowReceive, stream.Sink(), func(_ context.Context, c task.Control) task.Completion[int] {
					for range pressureObservations {
						if kind == "diagnostic" {
							c.Diagnostics.Emit(record)
						} else {
							c.Emit(progress)
						}
					}
					return task.Completion[int]{Settlement: task.Settlement{Outcome: task.OutcomeSuccess}}
				})
				if err != nil {
					b.Fatal(err)
				}
				if _, err := current.Wait(context.Background()); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/pressureObservations, "ns/event")
		})
	}
}
