package webrtc

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/framechannel"
)

func TestAggregateSendRetainsProviderBytesAndReservesControlCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		budget := NewSendCapacity()
		provider := newFakeDataChannel(pion.DataChannelStateOpen)
		channel := &Channel{dc: provider}
		budget.register(channel)
		provider.setBuffered(aggregateSendBytes - aggregateControlReserve)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			release, err := budget.reserve(ctx.Done(), ctx.Err, framechannel.MaxFrameSize, false)
			if release != nil {
				release()
			}
			done <- err
		}()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("bulk bypassed aggregate holdings: %v", err)
		default:
		}
		release, err := budget.reserve(t.Context().Done(), t.Context().Err, framechannel.MaxFrameSize, true)
		if err != nil {
			t.Fatal(err)
		}
		release()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if budget.reserved != 0 || budget.submissions != 0 {
			t.Fatal("cancellation leaked admission")
		}
		budget.remove(channel)
		release, err = budget.reserve(t.Context().Done(), t.Context().Err, framechannel.MaxFrameSize, false)
		if err != nil {
			t.Fatal(err)
		}
		release()
	})
}

func TestAggregateBurstLimitWakesIndependentLanes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		budget := NewSendCapacity()
		var releases []func()
		for range aggregateSubmissions {
			release, err := budget.reserve(t.Context().Done(), t.Context().Err, framechannel.MaxFrameSize, false)
			if err != nil {
				t.Fatal(err)
			}
			releases = append(releases, release)
		}
		done := make(chan func(), 1)
		go func() {
			release, _ := budget.reserve(t.Context().Done(), t.Context().Err, framechannel.MaxFrameSize, false)
			done <- release
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("aggregate burst escaped")
		default:
		}
		control, err := budget.reserve(t.Context().Done(), t.Context().Err, framechannel.MaxFrameSize, true)
		if err != nil {
			t.Fatal(err)
		}
		control()
		releases[0]()
		(<-done)()
		for _, release := range releases {
			release()
		}
		if budget.reserved != 0 || budget.submissions != 0 {
			t.Fatal("physical settlement leaked")
		}
	})
}
