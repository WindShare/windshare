package task

import (
	"context"
	"testing"
)

func TestChangesPublishJoinedResultsIndependentlyOfLossyEvidence(t *testing.T) {
	published, finish, joining, joined := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	config := testConfig(func(_ context.Context, control Control) Completion[int] {
		<-published
		for range 100 {
			control.Emit(detail{Value: 1})
		}
		control.NotifyStateChanged()
		<-finish
		return Completion[int]{Value: 42, Outcome: OutcomeSuccess}
	})
	config.Completed = func(error) { close(joining); <-joined }
	current, err := Start(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	changed := current.Changes()
	close(published)
	<-changed
	close(finish)
	<-joining
	select {
	case <-current.Done():
		t.Fatal("result became final before join")
	default:
	}
	changed = current.Changes()
	close(joined)
	<-changed
	result, err := current.Wait(context.Background())
	if err != nil || result.Value != 42 || result.Observations.CapacityDropped == 0 {
		t.Fatalf("%+v %v", result, err)
	}
}
