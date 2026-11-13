package relayv2_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/receiversession"
	"github.com/windshare/windshare/core/transfer/lanes"
)

// Keep one consumer demand waiting for network supply while the real relay
// sockets recover. Its readiness port prevents synthetic content from going on wire.
func holdRelaysetNetworkDemand(t *testing.T, ctx context.Context, runtime *receiversession.Runtime) func() {
	t.Helper()
	geometry, err := content.NewFileGeometry(1, catalog.MinChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(runtime.Descriptor().ShareInstance(), catalog.FileID{1}, content.FileRevision{1}, geometry, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	lifetime, cancel := context.WithCancel(ctx)
	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, err := runtime.LaneSet().Fetch(lifetime, lanes.Request{
			Demand:   lanes.Demand{LeaseID: content.LeaseID{1}, Descriptor: descriptor},
			Validate: func(records.BlockRecord) error { return nil },
			Ready: func() lanes.CapacityReadiness {
				select {
				case ready <- struct{}{}:
				default:
				}
				return lanes.CapacityReadiness{Kind: lanes.CapacityNetwork}
			},
		})
		done <- err
	}()
	stop := sync.OnceFunc(func() {
		cancel()
		if err := waitRelaysetEvent(t, ctx, done); !errors.Is(err, context.Canceled) {
			t.Errorf("network demand did not withdraw: %v", err)
		}
	})
	waitRelaysetEvent(t, ctx, ready)
	return stop
}
