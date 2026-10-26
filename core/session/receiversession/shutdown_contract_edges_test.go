package receiversession

import (
	"context"
	"testing"
	"time"
)

func TestWaitStoppedJoinsRuntimeThatClosedWithoutOrderedStop(t *testing.T) {
	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()

	sender.BeginClose()
	waitDone := make(chan error, 1)
	go func() { waitDone <- sender.WaitStopped(context.Background()) }()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("naturally closed wait-stop error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait-stop did not join naturally closed composite ownership")
	}
}
