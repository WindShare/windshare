package peerset

import (
	"context"
	"testing"

	sessioncontract "github.com/windshare/windshare/core/session"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
)

func TestThroughputRetirementCannotCancelAReplacementGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	path := &Path{ctx: ctx, cancel: cancel, lane: sessioncontract.LaneIdentity{ID: 2, Epoch: 3}, done: done}
	retireThroughputGeneration(path, transferlanes.Identity{ID: 2, Epoch: 2})
	if ctx.Err() != nil {
		t.Fatal("stale trial retired a replacement")
	}
	close(done)
	retireThroughputGeneration(path, transferlanes.Identity{ID: 2, Epoch: 3})
	if ctx.Err() == nil {
		t.Fatal("matching optional generation survived retirement")
	}
}
