package engine

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type resumeTestAuthority struct {
	snapshot RecoverySnapshot
	list     func(context.Context) (RecoverySnapshot, error)
	discard  func(context.Context, receivecontract.OperationID) (RecoveryDiscardReport, error)
}

func (authority resumeTestAuthority) List(ctx context.Context) (RecoverySnapshot, error) {
	if authority.list != nil {
		return authority.list(ctx)
	}
	return authority.snapshot, nil
}

func (authority resumeTestAuthority) Discard(ctx context.Context, id receivecontract.OperationID) (RecoveryDiscardReport, error) {
	return authority.discard(ctx, id)
}

func resumeTestOperation(t *testing.T) RecoveryOperation {
	t.Helper()
	id, err := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{0x31}, receivecontract.StableIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	return RecoveryOperation{ID: id, State: RecoveryIncomplete}
}

func TestRecoveryFacadeKeepsBoundIdentityAndFinalCleanupOutcome(t *testing.T) {
	application, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	closeErr := errors.New("output lease close failed")
	t.Cleanup(func() {
		if err := application.Close(context.Background()); !errors.Is(err, closeErr) {
			t.Errorf("shutdown lost authority release failure: %v", err)
		}
	})
	operation := resumeTestOperation(t)
	snapshot, err := NewRecoverySnapshot([]RecoveryOperation{operation}, false)
	if err != nil {
		t.Fatal(err)
	}
	authority := resumeTestAuthority{snapshot: snapshot, discard: func(_ context.Context, id receivecontract.OperationID) (RecoveryDiscardReport, error) {
		if id != operation.ID {
			t.Error("engine changed the selected operation")
		}
		return RecoveryDiscardReport{ID: id, Status: RecoveryDiscarded}, closeErr
	}}
	inventory, err := application.InspectRecovery(context.Background(), authority)
	if err != nil {
		t.Fatal(err)
	}
	if inventory.DiscardRestriction() != nil || inventory.CheckDiscard(operation.ID) != nil {
		t.Fatal("verified operation was refused")
	}
	view, err := inventory.Snapshot()
	if err != nil || len(view.Operations) != 1 {
		t.Fatalf("snapshot=%+v err=%v", view, err)
	}
	result := inventory.Discard(context.Background(), operation.ID)
	if result.Report.Status != RecoveryDiscarded || result.Successful() || !errors.Is(result.Err, closeErr) || !errors.Is(result.CleanupError, closeErr) {
		t.Fatalf("close outcome=%+v", result)
	}
	var inspected, discarded bool
	for envelope := range inventory.Observations() {
		if envelope.TaskID == "" {
			t.Fatal("recovery observation lost task identity")
		}
		if event, ok := envelope.Event.(RecoveryObservation); ok && event.Phase == RecoveryInspectCompleted {
			inspected = event.OperationCount == 1 && !event.Failed
		}
	}
	for envelope := range result.Observations {
		if envelope.TaskID == "" {
			t.Fatal("discard observation lost task identity")
		}
		if event, ok := envelope.Event.(RecoveryObservation); ok && event.Phase == RecoveryDiscardCompleted {
			discarded = event.OperationID == operation.ID && event.DiscardStatus == RecoveryDiscarded && event.Failed
		}
	}
	if !inspected || !discarded {
		t.Fatal("completed recovery observations lost final authority decisions")
	}
}

func TestEngineCloseCancelsAndJoinsRecoveryAuthorityBeforeReturning(t *testing.T) {
	for _, stage := range []string{"inventory", "discard"} {
		t.Run(stage, func(t *testing.T) {
			application, err := New(Config{})
			if err != nil {
				t.Fatal(err)
			}
			operation := resumeTestOperation(t)
			started := make(chan struct{})
			cancelled := make(chan struct{})
			release := make(chan struct{})
			recoveryDone := make(chan struct{})
			authority := resumeTestAuthority{snapshot: RecoverySnapshot{Operations: []RecoveryOperation{operation}}}
			block := func(ctx context.Context) {
				close(started)
				<-ctx.Done()
				close(cancelled)
				<-release
			}
			if stage == "inventory" {
				authority.list = func(ctx context.Context) (RecoverySnapshot, error) {
					block(ctx)
					return RecoverySnapshot{}, ctx.Err()
				}
				go func() {
					defer close(recoveryDone)
					if _, err := application.InspectRecovery(context.Background(), authority); !errors.Is(err, context.Canceled) {
						t.Errorf("cancelled inspection error=%v", err)
					}
				}()
			} else {
				authority.discard = func(ctx context.Context, id receivecontract.OperationID) (RecoveryDiscardReport, error) {
					block(ctx)
					return RecoveryDiscardReport{ID: id, Status: RecoveryDiscardCleanupPending}, ctx.Err()
				}
				inventory, err := application.InspectRecovery(context.Background(), authority)
				if err != nil {
					t.Fatal(err)
				}
				go func() {
					defer close(recoveryDone)
					result := inventory.Discard(context.Background(), operation.ID)
					if result.Report.Status != RecoveryDiscardCleanupPending || !errors.Is(result.Err, context.Canceled) {
						t.Errorf("cancelled discard=%+v", result)
					}
				}()
			}
			awaitResumeSignal(t, started)
			closed := make(chan error, 1)
			go func() { closed <- application.Close(context.Background()) }()
			awaitResumeSignal(t, cancelled)
			select {
			case err := <-closed:
				t.Fatalf("shutdown abandoned recovery authority: %v", err)
			default:
			}
			close(release)
			awaitResumeSignal(t, recoveryDone)
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown did not join recovery")
			}
			if _, err := application.InspectRecovery(context.Background(), authority); !errors.Is(err, ErrClosed) {
				t.Fatalf("closed engine admitted recovery: %v", err)
			}
		})
	}
}

func TestRecoveryFacadeRefusesDetachedAndClosedInventories(t *testing.T) {
	operation := resumeTestOperation(t)
	var detached *RecoveryInventory
	if _, err := detached.Snapshot(); !errors.Is(err, ErrRecoveryContract) {
		t.Fatalf("snapshot=%v", err)
	}
	if detached.DiscardRestriction() == nil || detached.CheckDiscard(operation.ID) == nil {
		t.Fatal("detached inventory accepted")
	}
	if result := detached.Discard(context.Background(), operation.ID); !errors.Is(result.Err, ErrRecoveryContract) {
		t.Fatalf("detached discard=%+v", result)
	}
	var absent *Engine
	if _, err := absent.InspectRecovery(context.Background(), nil); !errors.Is(err, ErrRecoveryContract) {
		t.Fatalf("nil engine=%v", err)
	}
	application, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	authority := resumeTestAuthority{snapshot: RecoverySnapshot{Operations: []RecoveryOperation{operation}}}
	inventory, err := application.InspectRecovery(context.Background(), authority)
	if err != nil {
		t.Fatal(err)
	}
	if err := application.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if result := inventory.Discard(context.Background(), operation.ID); !errors.Is(result.Err, ErrClosed) {
		t.Fatalf("closed inventory=%+v", result)
	}
}

func awaitResumeSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery did not reach expected lifecycle boundary")
	}
}

func TestRecoveryDiscardIncompleteReportRetainsGenericFailure(t *testing.T) {
	application := newTestEngine(t, Config{})
	operation := resumeTestOperation(t)
	authority := resumeTestAuthority{
		snapshot: RecoverySnapshot{Operations: []RecoveryOperation{operation}},
		discard: func(context.Context, receivecontract.OperationID) (RecoveryDiscardReport, error) {
			return RecoveryDiscardReport{ID: operation.ID, Status: RecoveryDiscardCleanupPending}, nil
		},
	}
	inventory, err := application.InspectRecovery(context.Background(), authority)
	if err != nil {
		t.Fatal(err)
	}
	result := inventory.Discard(context.Background(), operation.ID)
	if result.Successful() {
		t.Fatal("unfinished discard was marked successful")
	}
	finished := 0
	for observation := range result.Observations {
		if event, ok := observation.Event.(LifecycleObservation); ok && event.State == TaskFinished {
			finished++
			failure, classified := errors.AsType[*RecoveryFailure](event.Err)
			if event.Outcome != OutcomeFailed || event.FailureClass != FailureLocal || !classified || failure.Kind != RecoveryFailureNeedsAttention || !event.Settlement.Valid() {
				t.Fatalf("incomplete report lost its terminal failure: %+v", event)
			}
		}
	}
	if finished != 1 {
		t.Fatalf("finished observations = %d", finished)
	}
}

func TestRecoveryCancellationDoesNotHideIndependentFailure(t *testing.T) {
	cleanup := errors.New("authority close failed")
	for _, test := range []struct {
		cause, cleanup error
		outcome        Outcome
	}{
		{nil, nil, OutcomeSuccess},
		{context.Canceled, nil, OutcomeCancelled},
		{context.DeadlineExceeded, nil, OutcomeCancelled},
		{RecoveryDestinationFailure(context.Canceled), nil, OutcomeCancelled},
		{errors.Join(context.Canceled, errors.New("operation failed")), nil, OutcomeFailed},
		{context.Canceled, cleanup, OutcomeFailed},
	} {
		result := settleRecovery(test.cause, test.cleanup)
		if result.Outcome != test.outcome || !result.Valid() || result.CleanupError != test.cleanup {
			t.Fatalf("recovery settlement = %+v", result)
		}
		if test.cause != nil && !errors.Is(result.Err, test.cause) || test.cleanup != nil && !errors.Is(result.Err, test.cleanup) {
			t.Fatalf("recovery lost a cause: %+v", result)
		}
	}
}
