package e2e

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestRelayProxyBandwidthKeepsContentPendingAndCutInterruptsWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var target bytes.Buffer
		var forwarded atomic.Uint64
		writer := &relayDownstreamWriter{target: &target, bytes: &forwarded, ctx: ctx}
		payload := bytes.Repeat([]byte{1}, 2*relayCutWriteChunkBytes)
		done := make(chan error, 1)
		go func() {
			_, err := writer.Write(payload)
			done <- err
		}()
		synctest.Wait()
		if forwarded.Load() != 0 {
			t.Fatal("relay forwarded content before charging its bandwidth")
		}
		time.Sleep(time.Duration(relayCutWriteChunkBytes) * time.Second / relayCutBytesPerSecond)
		synctest.Wait()
		if forwarded.Load() != relayCutWriteChunkBytes {
			t.Fatalf("bandwidth boundary: forwarded=%d", forwarded.Load())
		}
		cancel()
		synctest.Wait()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cut did not interrupt bandwidth wait: %v", err)
		}
		if !bytes.Equal(target.Bytes(), payload[:relayCutWriteChunkBytes]) {
			t.Fatal("cut forwarded bytes that were still pending")
		}
	})
}
