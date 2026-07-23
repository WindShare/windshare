package liveshare

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
)

func TestPreparedSenderAndReceiverOwnSuite02Bootstrap(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file.bin"), []byte("live content"), 0o600); err != nil {
		t.Fatal(err)
	}
	sender, err := PrepareSender(context.Background(), SenderConfig{
		Paths: []string{root}, Relays: []string{"ws://127.0.0.1:8484"}, ChunkSize: catalog.MinChunkSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sender.Close() })
	if err := sender.AuthorizeRegistration(); err != nil {
		t.Fatal(err)
	}
	capability := sender.Capability()
	if capability.Suite != link.SuiteSenderAuthenticated || len(capability.PKHash) != link.PKHashBytes {
		t.Fatalf("capability = %+v", capability)
	}
	material := sender.Registration()
	if len(material.Descriptor) == 0 || len(material.SenderPrivateKey) == 0 || len(material.ShareInstance) != catalog.IdentityBytes {
		t.Fatalf("registration material = %+v", material)
	}
	receiver, err := PrepareReceiver(ReceiverConfig{Capability: capability, DescriptorObject: material.Descriptor})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(receiver.Close)
	if !bytes.Equal(receiver.Descriptor().ShareInstance().Bytes(), material.ShareInstance) ||
		receiver.Descriptor().SyntheticRoot().IsZero() {
		t.Fatal("receiver descriptor changed authenticated bootstrap identity")
	}
	copyOfCapability := sender.Capability()
	copyOfCapability.ReadSecret[0] ^= 0xff
	if sender.Capability().ReadSecret[0] == copyOfCapability.ReadSecret[0] {
		t.Fatal("sender leaked mutable capability secret storage")
	}
	recordSealer, catalogObjects := sender.recordSealer, sender.catalogObjects
	readSecretAlias, sessionAuthAlias, privateKeyAlias := sender.capability.ReadSecret, sender.sessionAuthKey, sender.privateKey
	if len(readSecretAlias) == 0 || len(sessionAuthAlias) == 0 || len(privateKeyAlias) == 0 ||
		bytes.Equal(readSecretAlias, make([]byte, len(readSecretAlias))) ||
		bytes.Equal(sessionAuthAlias, make([]byte, len(sessionAuthAlias))) ||
		bytes.Equal(privateKeyAlias, make([]byte, len(privateKeyAlias))) {
		t.Fatal("prepared sender did not populate observable parent secret aliases")
	}
	if err := sender.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := recordSealer.SealRevision(content.FileRevisionDescriptor{}); !errors.Is(err, records.ErrSealerDestroyed) {
		t.Fatalf("closed sender retained record sealer authority: %v", err)
	}
	if _, err := catalogObjects.LoadSealedPage(context.Background(), catalog.CatalogPage{}); !errors.Is(err, catalogflow.ErrSealedCatalogStoreDestroyed) {
		t.Fatalf("closed sender retained catalog sealer authority: %v", err)
	}
	if !bytes.Equal(readSecretAlias, make([]byte, len(readSecretAlias))) ||
		!bytes.Equal(sessionAuthAlias, make([]byte, len(sessionAuthAlias))) ||
		!bytes.Equal(privateKeyAlias, make([]byte, len(privateKeyAlias))) {
		t.Fatal("closed sender left secret bytes in retained parent aliases")
	}
	sender.mu.Lock()
	retainedOwnedGraph := sender.runtimeFactory != nil || sender.cache != nil || sender.catalogAccess != nil || sender.catalogObjects != nil ||
		sender.recordSealer != nil || sender.revisionStore != nil || sender.revisionSource != nil ||
		sender.catalogStore != nil || sender.selectedSource != nil || sender.keyTree != nil || sender.random != nil
	sender.mu.Unlock()
	if retainedOwnedGraph {
		t.Fatal("closed sender retained its destroyed resource graph")
	}
	if len(sender.Capability().ReadSecret) != 0 || len(sender.Registration().SenderPrivateKey) != 0 {
		t.Fatal("closed sender retained exposed secret material")
	}
	if err := sender.AuthorizeRegistration(); err == nil {
		t.Fatal("closed sender authorized relay registration")
	}
	if _, err := sender.NewRuntimeFactory(testRuntimeFactoryConfig(&testTerminalConnectivity{})); err == nil {
		t.Fatal("closed sender built a runtime factory")
	}
	if err := sender.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareReceiverRejectsSuite01(t *testing.T) {
	if _, err := PrepareReceiver(ReceiverConfig{Capability: link.Link{Suite: 0x01}}); err == nil {
		t.Fatal("suite-01 capability entered the production receiver")
	}
}

func TestLiveShareFacadeTransfersProgressiveDirectoryToDurableOutput(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("facade-transfer"), 300)
	if err := os.WriteFile(filepath.Join(root, "nested", "file.bin"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	sender, err := PrepareSender(context.Background(), SenderConfig{
		Paths: []string{root}, Relays: []string{"ws://127.0.0.1:8484"}, ChunkSize: catalog.MinChunkSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	receiver, err := PrepareReceiver(ReceiverConfig{
		Capability: sender.Capability(), DescriptorObject: sender.Registration().Descriptor,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	terminal := &testTerminalConnectivity{}
	config := testRuntimeFactoryConfig(terminal)
	factory, err := sender.NewRuntimeFactory(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sender.NewRuntimeFactory(config); err == nil {
		t.Fatal("sender built a second runtime factory over the same authority")
	}
	senderChannel, receiverChannel := newFacadeChannelPair()
	accepted := make(chan struct {
		runtime *sessionruntime.SenderRuntime
		err     error
	}, 1)
	go func() {
		runtime, err := factory.Accept(context.Background(), senderChannel)
		accepted <- struct {
			runtime *sessionruntime.SenderRuntime
			err     error
		}{runtime: runtime, err: err}
	}()
	receiverRuntime, err := receiver.Connect(context.Background(), receiverChannel)
	if err != nil {
		t.Fatal(err)
	}
	defer receiverRuntime.Close()
	acceptedResult := <-accepted
	if acceptedResult.err != nil {
		t.Fatal(acceptedResult.err)
	}
	defer acceptedResult.runtime.Close()
	outputRoot := t.TempDir()
	output, err := receiver.OpenOutput(context.Background(), outputRoot)
	if err != nil || output.Reopened {
		t.Fatalf("output = %+v, %v", output, err)
	}
	// Closing the facade ends admission, but the connected runtime must retain
	// its content-authentication resources until its own lifecycle ends.
	receiver.Close()
	rules, _ := transfer.NewSelectionRules(true, nil)
	job, err := receiverRuntime.NewTransferJob(rules, output.Session)
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != transfer.JobSucceeded || result.SucceededFiles != 1 {
		t.Fatalf("job result = %+v", result)
	}
	written, err := os.ReadFile(filepath.Join(outputRoot, "tree", "nested", "file.bin"))
	if err != nil || !bytes.Equal(written, payload) {
		t.Fatalf("output bytes = %d, %v", len(written), err)
	}
	if active := factory.ActiveSessions(); active != 1 {
		t.Fatalf("active sender sessions before facade close = %d", active)
	}
	if err := sender.Close(); err != nil {
		t.Fatal(err)
	}
	if active := factory.ActiveSessions(); active != 0 {
		t.Fatalf("active sender sessions after facade close = %d", active)
	}
	stops, cleanups := terminal.snapshot()
	if stops != 1 || cleanups != 1 {
		t.Fatalf("terminal calls = stop %d cleanup %d", stops, cleanups)
	}
}

func TestReceiverRuntimeResourcesDestroySecretsAfterLastLease(t *testing.T) {
	share := catalog.ShareInstance{1}
	keyTree, err := content.NewKeyTree(bytes.Repeat([]byte{2}, content.ReadSecretBytes), share)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, ed25519.SeedSize))
	verifier, err := catalogflow.NewCatalogObjectVerifier(catalogflow.CatalogObjectVerifierConfig{
		ShareInstance: share, CatalogKey: bytes.Repeat([]byte{4}, 32),
		SenderPublicKey: privateKey.Public().(ed25519.PublicKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	resources := newReceiverRuntimeResources(keyTree, verifier)
	first, err := resources.AcquireReceiverRuntimeResources()
	if err != nil {
		t.Fatal(err)
	}
	second, err := resources.AcquireReceiverRuntimeResources()
	if err != nil {
		t.Fatal(err)
	}

	resources.Close()
	if _, err := keyTree.CatalogKey(); err != nil {
		t.Fatalf("active lease lost key tree: %v", err)
	}
	if _, err := verifier.Verify(context.Background(), share, catalogflow.ListRequest{}, []byte{1}); errors.Is(err, catalogflow.ErrCatalogObjectVerifierDestroyed) {
		t.Fatal("active lease lost catalog verifier")
	}
	if _, err := resources.AcquireReceiverRuntimeResources(); !errors.Is(err, errReceiverClosed) {
		t.Fatalf("closed resources admitted a runtime: %v", err)
	}
	first.Release()
	if _, err := keyTree.CatalogKey(); err != nil {
		t.Fatalf("remaining lease lost key tree: %v", err)
	}
	second.Release()
	second.Release()
	if _, err := keyTree.CatalogKey(); !errors.Is(err, content.ErrKeyTreeDestroyed) {
		t.Fatalf("last lease did not destroy key tree: %v", err)
	}
	if _, err := verifier.Verify(context.Background(), share, catalogflow.ListRequest{}, []byte{1}); !errors.Is(err, catalogflow.ErrCatalogObjectVerifierDestroyed) {
		t.Fatalf("last lease did not destroy catalog verifier: %v", err)
	}
}

type testTerminalConnectivity struct {
	mu       sync.Mutex
	stops    int
	cleanups int
}

type testSenderPeerHandler struct{}

func (testSenderPeerHandler) HandleMessage(context.Context, protocolsession.Message) error {
	return nil
}
func (testSenderPeerHandler) Cancel(context.Context, protocolsession.OperationID) error { return nil }
func (testSenderPeerHandler) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func testRuntimeFactoryConfig(terminal sessionruntime.TerminalConnectivity) RuntimeFactoryConfig {
	return RuntimeFactoryConfig{
		TerminalConnectivity: terminal,
		PeerHandlers: sessionruntime.SenderPeerHandlerFactoryFunc(
			func(sessionruntime.SenderPeerSession) (sessionruntime.SenderPeerHandler, error) {
				return testSenderPeerHandler{}, nil
			},
		),
	}
}

func (connectivity *testTerminalConnectivity) StopRecovery() {
	connectivity.mu.Lock()
	connectivity.stops++
	connectivity.mu.Unlock()
}

func (connectivity *testTerminalConnectivity) Cleanup(context.Context) error {
	connectivity.mu.Lock()
	connectivity.cleanups++
	connectivity.mu.Unlock()
	return nil
}

func (connectivity *testTerminalConnectivity) snapshot() (int, int) {
	connectivity.mu.Lock()
	defer connectivity.mu.Unlock()
	return connectivity.stops, connectivity.cleanups
}

type facadePipe struct {
	mu     sync.Mutex
	inbox  [2]chan framechannel.Frame
	closed [2]bool
}

type facadeChannel struct {
	pipe  *facadePipe
	index int
}

func newFacadeChannelPair() (*facadeChannel, *facadeChannel) {
	pipe := &facadePipe{inbox: [2]chan framechannel.Frame{
		make(chan framechannel.Frame, 2_048), make(chan framechannel.Frame, 2_048),
	}}
	return &facadeChannel{pipe: pipe}, &facadeChannel{pipe: pipe, index: 1}
}

func (channel *facadeChannel) Send(ctx context.Context, frame framechannel.Frame) error {
	channel.pipe.mu.Lock()
	defer channel.pipe.mu.Unlock()
	target := 1 - channel.index
	if channel.pipe.closed[channel.index] || channel.pipe.closed[target] {
		return io.ErrClosedPipe
	}
	if err := ctx.Err(); err != nil {
		return ctx.Err()
	}
	channel.pipe.inbox[target] <- bytes.Clone(frame)
	return nil
}

func (channel *facadeChannel) SendTerminal(ctx context.Context, frame framechannel.Frame) error {
	return channel.Send(ctx, frame)
}

func (channel *facadeChannel) Recv() <-chan framechannel.Frame {
	return channel.pipe.inbox[channel.index]
}

func (channel *facadeChannel) State() framechannel.ChannelState {
	channel.pipe.mu.Lock()
	defer channel.pipe.mu.Unlock()
	if channel.pipe.closed[channel.index] {
		return framechannel.Closed
	}
	return framechannel.Open
}

func (channel *facadeChannel) Close() error {
	channel.pipe.mu.Lock()
	if !channel.pipe.closed[channel.index] {
		channel.pipe.closed[channel.index] = true
		close(channel.pipe.inbox[channel.index])
	}
	channel.pipe.mu.Unlock()
	return nil
}

func TestPreparedReceiverRejectsUseAfterClose(t *testing.T) {
	receiver := &PreparedReceiver{closed: true}
	if _, err := receiver.Connect(context.Background(), nil); err == nil {
		t.Fatal("closed receiver accepted a channel")
	}
	if _, err := receiver.OpenOutput(context.Background(), t.TempDir()); err == nil {
		t.Fatal("closed receiver opened output")
	}
	receiver.Close()
}

func TestPrepareSenderClosesEveryPartiallyBuiltAuthority(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "selected.bin")
	if err := os.WriteFile(filename, []byte("failure matrix"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, available := range []int{0, 32, 48, 64, 80, 96, 112, 124} {
		t.Run(fmt.Sprintf("random-bytes-%d", available), func(t *testing.T) {
			if sender, err := PrepareSender(context.Background(), SenderConfig{
				Paths: []string{filename}, Relays: []string{"ws://127.0.0.1:8484"},
				ChunkSize: catalog.MinChunkSize, Random: &budgetReader{remaining: available},
			}); err == nil {
				_ = sender.Close()
				t.Fatal("bounded random source unexpectedly completed preparation")
			}
		})
	}
	if _, err := PrepareSender(context.Background(), SenderConfig{}); err == nil {
		t.Fatal("empty sender config was accepted")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PrepareSender(cancelled, SenderConfig{Paths: []string{filename}, Relays: []string{"ws://relay"}}); err == nil {
		t.Fatal("cancelled preparation was accepted")
	}
	if _, err := PrepareSender(context.Background(), SenderConfig{
		Paths: []string{filename}, Relays: []string{"ws://relay"}, ChunkSize: catalog.MinChunkSize + 1,
	}); err == nil {
		t.Fatal("invalid chunk geometry was accepted")
	}
	if _, err := PrepareSender(context.Background(), SenderConfig{
		Paths: []string{filename}, Relays: []string{"ws://relay"}, Now: func() time.Time { return time.Unix(-1, 0) },
	}); err == nil {
		t.Fatal("non-portable descriptor creation time was accepted")
	}
	var nilSender *PreparedSender
	if err := nilSender.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareReceiverRejectsMalformedBootstrap(t *testing.T) {
	capability := link.Link{
		Suite: link.SuiteSenderAuthenticated, ReadSecret: bytes.Repeat([]byte{1}, link.ReadSecretBytes),
		PKHash: bytes.Repeat([]byte{2}, link.PKHashBytes), ShareID: "not-base64", Relays: []string{"ws://relay"},
	}
	if _, err := PrepareReceiver(ReceiverConfig{Capability: capability, DescriptorObject: []byte{1}}); err == nil {
		t.Fatal("malformed share identity was accepted")
	}
	shareID, _ := link.ShareIDForSenderKeyHash(capability.PKHash)
	capability.ShareID = shareID
	if _, err := PrepareReceiver(ReceiverConfig{Capability: capability, DescriptorObject: []byte{1}}); err == nil {
		t.Fatal("malformed descriptor object was accepted")
	}
	var nilReceiver *PreparedReceiver
	nilReceiver.Close()
}

type budgetReader struct{ remaining int }

func (reader *budgetReader) Read(destination []byte) (int, error) {
	if reader.remaining == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	count := min(len(destination), reader.remaining)
	for index := range count {
		destination[index] = byte(index + 1)
	}
	reader.remaining -= count
	if count != len(destination) {
		return count, io.ErrUnexpectedEOF
	}
	return count, nil
}
