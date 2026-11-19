package share

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/link"
)

func TestReadinessRetainsCapabilityOwnershipAcrossProviderAndCallerMutation(t *testing.T) {
	controller := NewController()
	original := Ready{Capability: link.Link{ReadSecret: []byte{1, 2}, PKHash: []byte{3, 4}, Relays: []string{"https://relay.example"}}}
	controller.publish(original)
	original.Capability.ReadSecret[0] = 5
	original.Capability.PKHash[0] = 6
	original.Capability.Relays[0] = "https://mutated.example"
	first, err := controller.Ready(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Capability.ReadSecret, []byte{1, 2}) || !bytes.Equal(first.Capability.PKHash, []byte{3, 4}) ||
		!slices.Equal(first.Capability.Relays, []string{"https://relay.example"}) {
		t.Fatal("provider mutation changed retained readiness")
	}
	first.Capability.ReadSecret[0] = 7
	first.Capability.PKHash[0] = 8
	first.Capability.Relays[0] = "https://consumer.example"
	second, err := controller.Ready(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(second.Capability.ReadSecret, []byte{1, 2}) || !bytes.Equal(second.Capability.PKHash, []byte{3, 4}) ||
		!slices.Equal(second.Capability.Relays, []string{"https://relay.example"}) {
		t.Fatal("consumer mutation changed retained readiness")
	}
}

func TestPublicationAndPreReadyCancellationSettleStartupOnce(t *testing.T) {
	controller := NewController()
	start := make(chan struct{})
	published, cancelled := make(chan bool, 1), make(chan bool, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		<-start
		published <- controller.publish(Ready{})
	}()
	go func() {
		<-start
		cancelled <- controller.CancelBeforeReady(cancel)
	}()
	close(start)

	didPublish, didCancel := <-published, <-cancelled
	if didPublish == didCancel {
		t.Fatal("publication and cancellation must have exactly one winner")
	}
	_, err := controller.Ready(t.Context())
	if didPublish && (err != nil || ctx.Err() != nil) {
		t.Fatal("a published share was withdrawn", err)
	}
	if didCancel && (!errors.Is(err, context.Canceled) || ctx.Err() != context.Canceled) {
		t.Fatal("a withdrawn attempt remained publishable", err)
	}
}
