package receive

import (
	"context"
	"errors"
	"github.com/windshare/windshare/connectivity/relayset"
	"testing"
)

func TestGetReceiverRecoveryOptionsFailure(t *testing.T) {
	observation, _ := newTestObservation(t)
	app := &runner{receiverRecoveryOptions: relayset.ReceiverRecoveryOptions{InitialWait: -1}}
	if _, code := app.connectGetReceiver(context.Background(), getRequest{}, observation); code != stepLocalFailure {
		t.Fatal(code)
	}
	recovery, _ := relayset.NewReceiverRecovery(relayset.ReceiverRecoveryOptions{})
	owner := &getReceiverRecovery{owner: recovery}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := owner.replace(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
