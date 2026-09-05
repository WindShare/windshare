package relayset

import (
	"context"
	"errors"
	"testing"
)

func TestFailedDialCannotLeakReturnedEndpoint(t *testing.T) {
	e := newEndpoint()
	failed := errors.New("registration failed after resource creation")
	set, _ := NewSender(context.Background(), []string{"relay"}, func(context.Context, string) (SenderEndpoint, error) { return e, failed })
	if err := set.WaitReady(context.Background()); !errors.Is(err, failed) {
		t.Fatal(err)
	}
	cleanupSet(t, set)
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.cleaned || !e.stopped {
		t.Fatal("partial dial ownership leaked")
	}
}
func TestNilSuccessfulDialIsRejected(t *testing.T) {
	set, _ := NewSender(context.Background(), []string{"relay"}, func(context.Context, string) (SenderEndpoint, error) { return nil, nil })
	defer cleanupSet(t, set)
	if !errors.Is(set.WaitReady(context.Background()), ErrConfig) {
		t.Fatal("nil endpoint published readiness")
	}
}
