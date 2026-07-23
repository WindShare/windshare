package liveshare

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

type beginReceiverCloseOnSendChannel struct {
	framechannel.Channel
	once   sync.Once
	onSend func()
}

func (channel *beginReceiverCloseOnSendChannel) Send(ctx context.Context, frame framechannel.Frame) error {
	channel.once.Do(channel.onSend)
	return channel.Channel.Send(ctx, frame)
}

func TestPreparedReceiverConcurrentCloseJoinsConnectAdmissionBeforeSecretTeardown(t *testing.T) {
	receiver := newPreparedReceiverForLifecycleTest(t)
	receiver.mu.Lock()
	resources := receiver.resources
	factory := receiver.factory
	receiver.mu.Unlock()
	resources.mu.Lock()
	keyTree := resources.keyTree
	resources.mu.Unlock()
	receiverChannel, peerChannel := newFacadeChannelPair()
	t.Cleanup(func() {
		_ = receiverChannel.Close()
		_ = peerChannel.Close()
	})
	connectResult := make(chan error, 1)
	go func() {
		_, connectErr := receiver.Connect(context.Background(), receiverChannel)
		connectResult <- connectErr
	}()
	select {
	case <-peerChannel.Recv():
	case <-time.After(time.Second):
		t.Fatal("receiver did not reach the server-hello wait")
	}
	firstClose := make(chan struct{})
	secondClose := make(chan struct{})
	go func() { receiver.Close(); close(firstClose) }()
	go func() { receiver.Close(); close(secondClose) }()
	if err := <-connectResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("facade-canceled Connect error=%v", err)
	}
	for index, completed := range []<-chan struct{}{firstClose, secondClose} {
		select {
		case <-completed:
		case <-time.After(time.Second):
			t.Fatalf("concurrent Close %d did not join", index)
		}
	}
	if _, err := keyTree.CatalogKey(); !errors.Is(err, content.ErrKeyTreeDestroyed) {
		t.Fatalf("closed facade retained idle receiver key tree: %v", err)
	}
	receiver.mu.Lock()
	retainedFactory, retainedResources := receiver.factory, receiver.resources
	receiver.mu.Unlock()
	if retainedFactory != nil || retainedResources != nil {
		t.Fatal("closed facade retained its factory or secret-resource graph")
	}
	if _, err := receiver.Connect(context.Background(), receiverChannel); !errors.Is(err, errReceiverClosed) {
		t.Fatalf("facade Connect after Close error=%v", err)
	}
	if _, err := factory.Connect(context.Background(), receiverChannel); !errors.Is(err, sessionruntime.ErrRuntimeClosed) {
		t.Fatalf("factory Connect after facade Close error=%v", err)
	}
}

func TestPreparedReceiverBeginCloseIsSafeFromConnectSendCallback(t *testing.T) {
	receiver := newPreparedReceiverForLifecycleTest(t)
	base, peer := newFacadeChannelPair()
	t.Cleanup(func() {
		_ = base.Close()
		_ = peer.Close()
	})
	callbackReturned := make(chan struct{})
	channel := &beginReceiverCloseOnSendChannel{Channel: base, onSend: func() {
		receiver.BeginClose()
		close(callbackReturned)
	}}
	connectResult := make(chan error, 1)
	go func() {
		_, connectErr := receiver.Connect(context.Background(), channel)
		connectResult <- connectErr
	}()
	select {
	case <-callbackReturned:
	case <-time.After(time.Second):
		t.Fatal("Connect Send callback deadlocked reentrant BeginClose")
	}
	select {
	case err := <-connectResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("reentrant BeginClose Connect error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Connect did not drain after callback BeginClose")
	}
	closed := make(chan struct{})
	go func() { receiver.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("owner Close did not join callback-initiated close")
	}
	receiver.mu.Lock()
	resumeIntent := receiver.resumeIntent
	receiver.mu.Unlock()
	if resumeIntent != (osfs.OutputResumeIntent{}) {
		t.Fatal("closed receiver retained capability-derived resume authority")
	}
}

func newPreparedReceiverForLifecycleTest(t *testing.T) *PreparedReceiver {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source.bin"), []byte("receiver lifecycle"), 0o600); err != nil {
		t.Fatal(err)
	}
	sender, err := PrepareSender(context.Background(), SenderConfig{
		Paths: []string{root}, Relays: []string{"ws://127.0.0.1:8484"}, ChunkSize: catalog.MinChunkSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sender.Close() })
	receiver, err := PrepareReceiver(ReceiverConfig{
		Capability: sender.Capability(), DescriptorObject: sender.Registration().Descriptor,
	})
	if err != nil {
		t.Fatal(err)
	}
	return receiver
}
