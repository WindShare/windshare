package turnpath

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/windshare/windshare/core/session/turncontrol"
)

type slowClosingReceiverPath struct {
	entered, release, done chan struct{}
}

func (path *slowClosingReceiverPath) Lane() (turncontrol.Lane, bool) {
	return turncontrol.Lane{}, false
}
func (path *slowClosingReceiverPath) Done() <-chan struct{} { return path.done }
func (path *slowClosingReceiverPath) Close() error {
	close(path.entered)
	<-path.release
	close(path.done)
	return nil
}

func TestReceiverWithdrawsIngressBeforeJoiningPathCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := &slowClosingReceiverPath{entered: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
		t.Cleanup(func() { close(path.release) })
		r := NewReceiver(ReceiverConfig{SessionID: [16]byte{1}})
		r.paths[[16]byte{3}] = &receiverPath{path: path}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		stopped := make(chan struct{})
		go func() { r.Run(ctx); close(stopped) }()
		synctest.Wait()
		select {
		case <-path.entered:
		default:
			t.Fatal("receiver did not begin path cleanup")
		}
		err := r.Handle(t.Context(), controlBody(t, turncontrol.Available{
			Authority: turncontrol.Header{SessionID: [16]byte{1}, OwnerPathID: [16]byte{2}, Sequence: 1},
		}))
		if !errors.Is(err, errControlStopped) {
			t.Fatalf("closing receiver retained a command without a consumer: %v", err)
		}
	})
}

type changingReceiverPath struct {
	receiverFixturePath
	lane turncontrol.Lane
	live bool
}

func (path *changingReceiverPath) Lane() (turncontrol.Lane, bool) { return path.lane, path.live }

func TestReceiverReadinessFollowsLiveLaneIncarnations(t *testing.T) {
	var sent []turncontrol.Ready
	fail := true
	r := NewReceiver(ReceiverConfig{SessionID: [16]byte{1}, Send: func(_ context.Context, body []byte) error {
		message, err := turncontrol.Decode(body)
		if err != nil {
			return err
		}
		if ready, ok := message.(turncontrol.Ready); ok {
			sent = append(sent, ready)
			if fail {
				return errors.New("control lane unavailable")
			}
		}
		return nil
	}})
	path := &changingReceiverPath{
		receiverFixturePath: receiverFixturePath{done: make(chan struct{})},
		lane:                turncontrol.Lane{ID: 7, Epoch: 1}, live: true,
	}
	entry := &receiverPath{path: path, grant: turncontrol.Grant{
		Authority: turncontrol.Header{SessionID: [16]byte{1}, OwnerPathID: [16]byte{2}, Sequence: 1}, PathID: [16]byte{3},
	}}
	r.notifyReady(t.Context(), entry)
	path.live = false
	r.notifyReady(t.Context(), entry)
	if len(sent) != 1 {
		t.Fatal("ended lane continued retrying readiness")
	}
	path.lane.Epoch++
	path.live = true
	fail = false
	r.notifyReady(t.Context(), entry)
	r.notifyReady(t.Context(), entry)
	if len(sent) != 2 || sent[0].Lane.Epoch != 1 || sent[1].Lane.Epoch != 2 {
		t.Fatal("new incarnation reused readiness from the old attempt")
	}
	_ = path.Close()
	r.notifyReady(t.Context(), entry)
	if len(sent) != 2 {
		t.Fatal("closed path announced readiness")
	}
}
