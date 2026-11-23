package peerset

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session"
	p "github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/lanes"
)

type turnTestRuntime struct {
	attached int
	detached chan session.LaneIdentity
	lost     chan struct{}
}

func (*turnTestRuntime) SetTURNControlHandler(func(context.Context, []byte) error) {}
func (*turnTestRuntime) SendTURNControl(context.Context, []byte) error             { return nil }
func (*turnTestRuntime) LaneSet() *lanes.Set                                       { return nil }
func (r *turnTestRuntime) AttachedLanes() int                                      { return r.attached }
func (r *turnTestRuntime) LaneDone(session.LaneIdentity) (<-chan struct{}, bool)   { return r.lost, true }
func (r *turnTestRuntime) DetachLane(lane session.LaneIdentity) bool {
	r.detached <- lane
	return true
}

type testTURNAttempt struct {
	*fakeAttempt
	binding v2signal.Binding
	id      uint32
}

func (a *testTURNAttempt) Lane() (session.LaneIdentity, bool) {
	return session.LaneIdentity{ID: a.id, Epoch: 1}, signalled(a.ready)
}

func TestTURNRenewalProtectsTheLastLaneUntilItsSuccessorAttaches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		owner, _ := New(Config{})
		receiver := &ReceiverSession{owner: owner, ctx: t.Context()}
		runtime := &turnTestRuntime{attached: 2, detached: make(chan session.LaneIdentity, 8), lost: make(chan struct{})}
		manager := newTURNManager(receiver, runtime)
		defer manager.close()
		var attempts []*testTURNAttempt
		manager.negotiate = func(_ context.Context, binding v2signal.Binding, _ p.TURNGrant) (Attempt, error) {
			attempt := &testTURNAttempt{fakeAttempt: newAttempt(), binding: binding, id: uint32(len(attempts) + 1)}
			attempts = append(attempts, attempt)
			return attempt, nil
		}
		grant := p.TURNGrant{ServiceID: "home", GrantID: p.TURNGrantID{1}, ValidForMS: 60000}
		fresh := func(target string) *testTURNAttempt {
			manager.serial++
			manager.wait[manager.serial] = requestState{mode: p.TURNAll, service: target}
			manager.accept(time.Now(), p.TURNGrants{RequestID: manager.serial, Status: p.TURNOK, Grants: []p.TURNGrant{grant}})
			synctest.Wait()
			return attempts[len(attempts)-1]
		}
		step := func() { manager.step(time.Now(), true, lanes.SchedulingDemand{}); synctest.Wait() }
		admit := func(a *testTURNAttempt) { close(a.ready); synctest.Wait(); step() }
		first := fresh("")
		admit(first)
		second := fresh("home")
		if second.binding.PeerPathID != first.binding.PeerPathID || second.binding.PeerSlot != first.binding.PeerSlot || second.binding.AttemptSequence != 2 {
			t.Fatal("ordinary renewal lost the service path or sequence watermark")
		}
		if (<-runtime.detached).ID != first.id {
			t.Fatal("ordinary renewal failed to retire its previous lane")
		}
		admit(second)
		runtime.attached = 1
		failed := fresh("home")
		failed.Close()
		synctest.Wait()
		step()
		if len(runtime.detached) != 0 {
			t.Fatal("failed replacement retired the last lane")
		}
		spare := fresh("home")
		if spare.binding.PeerSlot == second.binding.PeerSlot || spare.binding.PeerPathID == second.binding.PeerPathID ||
			spare.binding.PeerPathID != failed.binding.PeerPathID || spare.binding.AttemptSequence != 2 {
			t.Fatal("replacement lost its spare slot watermark")
		}
		admit(spare)
		if (<-runtime.detached).ID != second.id {
			t.Fatal("authenticated successor did not retire its predecessor")
		}
		close(runtime.lost)
		synctest.Wait()
		step()
		if (<-runtime.detached).ID != spare.id {
			t.Fatal("lost TURN lane remained attached")
		}
		manager.reset()
	})
}
