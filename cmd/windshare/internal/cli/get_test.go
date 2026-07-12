package cli

import (
	"context"
	"testing"
)

func TestGetCanceledBeforeDialIsNotNetworkFailure(t *testing.T) {
	capability := testLink()
	capability.Relays = []string{"ws://127.0.0.1:1"}
	url, err := capability.URL(DefaultFrontURL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	app, _, _ := newTestApp("")
	if got := app.Run(ctx, []string{"get", url}); got != ExitFailure {
		t.Fatalf("canceled get exit = %d, want %d", got, ExitFailure)
	}
}
