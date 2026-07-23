package sessionruntime

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestRuntimeDoneFollowsLaneCryptoDestructionAndReferenceRelease(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	runtime.lanes.mu.Lock()
	lane := runtime.lanes.active[runtime.initial.ID]
	runtime.lanes.mu.Unlock()
	if lane == nil || lane.sealer == nil || lane.opener == nil {
		t.Fatal("runtime did not retain its initial lane crypto owners")
	}
	sealer, opener, writer := lane.sealer, lane.opener, lane.writer

	runtime.start()
	runtime.beginClose()
	runtime.waitClosed()

	select {
	case <-writer.Done():
	default:
		t.Fatal("runtime released lane crypto before its writer joined")
	}
	select {
	case <-lane.done:
	default:
		t.Fatal("runtime completed before its pump/writer lane join")
	}
	if lane.sealer != nil || lane.opener != nil || lane.writer != nil ||
		lane.pump != nil || lane.channel != nil || lane.ctx != nil || lane.cancel != nil {
		t.Fatalf("closed runtime lane retained dependency references: %+v", lane)
	}
	if _, err := sealer.Seal(nil); !errors.Is(err, protocolsession.ErrEnvelopeClosed) {
		t.Fatalf("post-Done sealer use error = %v", err)
	}
	if _, err := opener.Open(nil); !errors.Is(err, protocolsession.ErrEnvelopeClosed) {
		t.Fatalf("post-Done opener use error = %v", err)
	}
}

type reentrantLaneCloseChannel struct {
	protocolsession.FrameChannel
	onClose func()
	once    sync.Once
}

func (channel *reentrantLaneCloseChannel) Close() error {
	channel.once.Do(channel.onClose)
	return channel.FrameChannel.Close()
}

func TestRejectedLaneCloseMayReenterRegistryInspection(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	runtime.lanes.mu.Lock()
	runtime.lanes.stopping = true
	runtime.lanes.mu.Unlock()
	base, peer := newMemoryChannelPair()
	t.Cleanup(func() { _ = peer.Close() })
	reentered := make(chan struct{})
	channel := &reentrantLaneCloseChannel{
		FrameChannel: base,
		onClose: func() {
			_ = runtime.lanes.len()
			close(reentered)
		},
	}
	result := make(chan error, 1)
	go func() {
		_, err := runtime.lanes.add(
			LaneIdentity{ID: 2, Epoch: 1},
			channel,
			permissiveInboundAuthenticator(),
			false,
		)
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, ErrRuntimeClosed) {
			t.Fatalf("rejected lane error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("reentrant FrameChannel.Close deadlocked on the lane registry")
	}
	select {
	case <-reentered:
	default:
		t.Fatal("lane rejection did not close its channel")
	}
}

func TestNaturalLaneDetachReleasesRetainedLaneReferences(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	runtime.lanes.mu.Lock()
	initial := runtime.lanes.active[runtime.initial.ID]
	runtime.lanes.mu.Unlock()
	sealer, opener := initial.sealer, initial.opener
	secondaryChannel, secondaryPeer := newMemoryChannelPair()
	t.Cleanup(func() { _ = secondaryPeer.Close() })
	if _, err := runtime.lanes.add(
		LaneIdentity{ID: 2, Epoch: 1},
		secondaryChannel,
		permissiveInboundAuthenticator(),
		false,
	); err != nil {
		t.Fatal(err)
	}
	runtime.start()
	if !runtime.lanes.detach(initial.identity) {
		t.Fatal("live initial lane did not detach")
	}
	if initial.sealer != nil || initial.opener != nil || initial.writer != nil ||
		initial.pump != nil || initial.channel != nil || initial.ctx != nil || initial.cancel != nil {
		t.Fatalf("naturally detached lane retained dependency references: %+v", initial)
	}
	if _, err := sealer.NextSequence(); !errors.Is(err, protocolsession.ErrEnvelopeClosed) {
		t.Fatalf("detached sealer use error = %v", err)
	}
	if _, err := opener.NextSequence(); !errors.Is(err, protocolsession.ErrEnvelopeClosed) {
		t.Fatalf("detached opener use error = %v", err)
	}
	select {
	case <-runtime.ctx.Done():
		t.Fatal("detaching one lane closed a runtime with another usable lane")
	default:
	}
	runtime.beginClose()
	runtime.waitClosed()
}

func TestUnstartedLaneDetachSynchronouslyReleasesOwnedDependencies(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	detachCalls := 0
	var detachedIdentity LaneIdentity
	runtime.lanes.setDetachHook(func(identity LaneIdentity) {
		detachCalls++
		detachedIdentity = identity
	})
	channel, peer := newMemoryChannelPair()
	t.Cleanup(func() { _ = peer.Close() })
	identity := LaneIdentity{ID: runtime.initial.ID + 1, Epoch: 1}
	lane, err := runtime.lanes.add(
		identity, channel, permissiveInboundAuthenticator(), false,
	)
	if err != nil {
		t.Fatal(err)
	}
	sealer, opener := lane.sealer, lane.opener
	if sealer == nil || opener == nil {
		t.Fatal("unstarted lane did not own its crypto dependencies")
	}

	if !runtime.lanes.detach(identity) {
		t.Fatal("unstarted admitted lane did not detach")
	}
	if detachCalls != 1 || detachedIdentity != identity {
		t.Fatalf("unstarted detach hook calls=%d identity=%+v, want one for %+v",
			detachCalls, detachedIdentity, identity)
	}
	if runtime.lanes.detach(identity) || detachCalls != 1 {
		t.Fatalf("repeated unstarted detach result changed hook count=%d", detachCalls)
	}
	if lane.sealer != nil || lane.opener != nil || lane.writer != nil ||
		lane.pump != nil || lane.channel != nil || lane.ctx != nil || lane.cancel != nil {
		t.Fatalf("unstarted detached lane retained dependency references: %+v", lane)
	}
	if _, err := sealer.NextSequence(); !errors.Is(err, protocolsession.ErrEnvelopeClosed) {
		t.Fatalf("unstarted detached sealer use error = %v", err)
	}
	if _, err := opener.NextSequence(); !errors.Is(err, protocolsession.ErrEnvelopeClosed) {
		t.Fatalf("unstarted detached opener use error = %v", err)
	}
	select {
	case <-lane.done:
	default:
		t.Fatal("unstarted detach did not publish lane completion")
	}
	select {
	case <-runtime.ctx.Done():
		t.Fatal("detaching an unstarted secondary lane closed the runtime")
	default:
	}
}

func TestUnstartedLastLaneDetachPublishesHookAndCancelsRuntime(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	runtime.lanes.mu.Lock()
	lane := runtime.lanes.active[runtime.initial.ID]
	runtime.lanes.mu.Unlock()
	if lane == nil {
		t.Fatal("initial unstarted lane is missing")
	}
	detachCalls := 0
	var detachedIdentity LaneIdentity
	runtime.lanes.setDetachHook(func(identity LaneIdentity) {
		detachCalls++
		detachedIdentity = identity
	})

	if !runtime.lanes.detach(runtime.initial) {
		t.Fatal("last unstarted lane did not detach")
	}
	if detachCalls != 1 || detachedIdentity != runtime.initial {
		t.Fatalf("last-lane hook calls=%d identity=%+v, want one for %+v",
			detachCalls, detachedIdentity, runtime.initial)
	}
	select {
	case <-runtime.ctx.Done():
	default:
		t.Fatal("last unstarted lane detach did not cancel the runtime")
	}
	if lane.writer != nil || lane.channel != nil || lane.sealer != nil || lane.opener != nil {
		t.Fatal("last unstarted lane cancellation retained owned dependencies")
	}
	if runtime.lanes.detach(runtime.initial) || detachCalls != 1 {
		t.Fatalf("repeated last-lane detach changed hook count=%d", detachCalls)
	}
}

func TestRuntimeConstructionAbortDestroysUnstartedLaneCrypto(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	runtime.lanes.mu.Lock()
	lane := runtime.lanes.active[runtime.initial.ID]
	runtime.lanes.mu.Unlock()
	sealer, opener := lane.sealer, lane.opener

	runtime.abortBeforeStart()

	if lane.sealer != nil || lane.opener != nil || lane.writer != nil ||
		lane.pump != nil || lane.channel != nil {
		t.Fatalf("aborted runtime lane retained dependency references: %+v", lane)
	}
	if _, err := sealer.NextSequence(); !errors.Is(err, protocolsession.ErrEnvelopeClosed) {
		t.Fatalf("aborted sealer use error = %v", err)
	}
	if _, err := opener.NextSequence(); !errors.Is(err, protocolsession.ErrEnvelopeClosed) {
		t.Fatalf("aborted opener use error = %v", err)
	}
}
