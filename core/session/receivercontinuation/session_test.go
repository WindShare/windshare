package receivercontinuation

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/internal/testoutputroot"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/core/transfer/revisionwait"
)

type fixture struct {
	t        *testing.T
	sender   *liveshare.PreparedSender
	factory  *sessionruntime.SenderFactory
	filename string
	payload  []byte
}

func newFixture(t *testing.T, directory ...bool) *fixture {
	t.Helper()
	owner, err := revisioncapacity.NewProcessOwner(revisioncapacity.DefaultProcessConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := owner.Close(); err != nil {
			t.Error(err)
		}
	})
	filename := filepath.Join(t.TempDir(), "source.bin")
	payload := bytes.Repeat([]byte("authenticated continuation"), 2000)
	if err = os.WriteFile(filename, payload, 0600); err != nil {
		t.Fatal(err)
	}
	source := filename
	if len(directory) != 0 && directory[0] {
		source = filepath.Dir(filename)
	}
	sender, err := liveshare.PrepareSender(context.Background(), liveshare.SenderConfig{Paths: []string{source}, Relays: []string{"ws://local.invalid"}, ChunkSize: catalog.MinChunkSize, RevisionCapacity: owner.Coordinator()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sender.Close(); err != nil {
			t.Error(err)
		}
	})
	factory, err := sender.NewRuntimeFactory(liveshare.RuntimeFactoryConfig{TerminalConnectivity: terminal{}, PeerHandlers: sessionruntime.SenderPeerHandlerFactoryFunc(func(sessionruntime.SenderPeerSession) (sessionruntime.SenderPeerHandler, error) { return peer{}, nil })})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, sender: sender, factory: factory, filename: filename, payload: payload}
}
func (f *fixture) connect() (*sessionruntime.ReceiverRuntime, *pipeChannel) {
	f.t.Helper()
	prepared, err := liveshare.PrepareReceiver(liveshare.ReceiverConfig{Capability: f.sender.Capability(), DescriptorObject: f.sender.Registration().Descriptor})
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(prepared.Close)
	a, b := newPipe()
	accepted := make(chan error, 1)
	go func() {
		senderRuntime, err := f.factory.Accept(context.Background(), a)
		b.sender = senderRuntime
		accepted <- err
	}()
	runtime, err := prepared.Connect(context.Background(), b, transfer.LaneRouteRelay)
	if err != nil {
		f.t.Fatal(err)
	}
	if err = <-accepted; err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(runtime.Close)
	return runtime, b
}

type terminal struct{}

func (terminal) StopRecovery()                 {}
func (terminal) Cleanup(context.Context) error { return nil }
func (terminal) Stop(context.Context) error    { return nil }

type peer struct{}

func (peer) HandleMessage(context.Context, protocolsession.Message) error { return nil }
func (peer) Cancel(context.Context, protocolsession.OperationID) error    { return nil }
func (peer) Run(ctx context.Context) error                                { <-ctx.Done(); return ctx.Err() }

type pipe struct {
	mu     sync.Mutex
	closed bool
	inbox  [2]chan framechannel.Frame
}
type pipeChannel struct {
	pipe   *pipe
	index  int
	sender *sessionruntime.SenderRuntime
}

func newPipe() (*pipeChannel, *pipeChannel) {
	p := &pipe{inbox: [2]chan framechannel.Frame{make(chan framechannel.Frame, 2048), make(chan framechannel.Frame, 2048)}}
	return &pipeChannel{pipe: p}, &pipeChannel{pipe: p, index: 1}
}
func (c *pipeChannel) Send(ctx context.Context, f framechannel.Frame) error {
	c.pipe.mu.Lock()
	defer c.pipe.mu.Unlock()
	if c.pipe.closed {
		return io.ErrClosedPipe
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case c.pipe.inbox[1-c.index] <- bytes.Clone(f):
		return nil
	}
}
func (c *pipeChannel) SendTerminal(ctx context.Context, f framechannel.Frame) error {
	return c.Send(ctx, f)
}
func (c *pipeChannel) Recv() <-chan framechannel.Frame { return c.pipe.inbox[c.index] }
func (c *pipeChannel) State() framechannel.ChannelState {
	c.pipe.mu.Lock()
	defer c.pipe.mu.Unlock()
	if c.pipe.closed {
		return framechannel.Closed
	}
	return framechannel.Open
}
func (c *pipeChannel) Close() error {
	c.pipe.mu.Lock()
	defer c.pipe.mu.Unlock()
	if !c.pipe.closed {
		c.pipe.closed = true
		close(c.pipe.inbox[0])
		close(c.pipe.inbox[1])
	}
	return nil
}

func TestTransferKeepsOutputAndConfirmedProgressAcrossFreshSession(t *testing.T) {
	f := newFixture(t)
	runtime, channel := f.connect()
	var replacements atomic.Int32
	continuation, err := New(context.Background(), runtime, func(context.Context, *sessionruntime.ReceiverRuntime) (*sessionruntime.ReceiverRuntime, error) {
		replacements.Add(1)
		next, _ := f.connect()
		return next, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer continuation.Close()
	out := testoutputroot.New(t)
	output, err := osfs.NewFilesystemOutputAuthority(osfs.FilesystemOutputAuthorityConfig{RootPath: out.RootPath, CreateRoot: out.CreateRoot})
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	rules, _ := transfer.NewSelectionRules(true, nil)
	selection, err := transfer.NewSelectionSpec(runtime.Descriptor().ShareInstance(), runtime.Descriptor().SyntheticRoot(), rules)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := output.ReserveDirectTree(context.Background(), selection, receivecontract.NewCatalogRootDirectoryTree())
	if err != nil {
		t.Fatal(err)
	}
	intent, ok := reservation.ReceiveIntent()
	if !ok {
		t.Fatal("missing reservation")
	}
	id, _ := transfer.NewTransferJobID()
	var reads []content.Range
	reader := rangeReaderFunc(func(ctx context.Context, lease content.LeaseID, descriptor content.FileRevisionDescriptor, requested content.Range, sink transfer.RangeSink) error {
		reads = append(reads, requested)
		err := continuation.ReadRange(ctx, lease, descriptor, requested, sink)
		if len(reads) == 1 && err == nil {
			_ = channel.Close()
			<-runtime.Done()
		}
		return err
	})
	liveOutput := &singleOpenMaterializer{materializer: output}
	job, err := transfer.NewTransferJob(transfer.TransferJobConfig{ReceiveIntent: intent, JobID: id, Session: continuation, Catalog: continuation, Revisions: continuation, Blocks: reader, Materializer: liveOutput, RevisionWait: &revisionwait.Config{GenerationFence: continuation}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := job.Run(ctx)
	if result.Outcome != transfer.DirectTreeOutcomeSuccess {
		t.Fatalf("outcome=%v cause=%v settlement=%v reads=%v replacements=%d", result.Outcome, result.TerminationCause, result.SettlementFailure, reads, replacements.Load())
	}
	if replacements.Load() != 1 || len(reads) < 2 || reads[1].Offset != reads[0].End {
		t.Fatalf("replacement=%d ranges=%v", replacements.Load(), reads)
	}
	if continuation.Runtime().ProtocolSessionID().Equal(runtime.ProtocolSessionID()) {
		t.Fatal("retired session revived")
	}
	destination, _ := intent.MaterializationPlan().DestinationReservation()
	written, err := os.ReadFile(filepath.Join(out.RootPath, destination.PhysicalName(), "source.bin"))
	if err != nil || !bytes.Equal(written, f.payload) {
		t.Fatalf("output=%d err=%v", len(written), err)
	}
	if len(continuation.leases) != 0 {
		t.Fatal("fresh leases not released")
	}
	if liveOutput.opens != 1 || liveOutput.session.starts != 1 {
		t.Fatalf("output reopened: roots=%d files=%d", liveOutput.opens, liveOutput.session.starts)
	}
}

// This consumer deliberately exposes no operation-reopen API and advertises no
// crash durability. A network generation must not demand either capability.
type singleOpenMaterializer struct {
	materializer transfer.DirectTreeMaterializer
	opens        int
	session      *singleFileSession
}

func (m *singleOpenMaterializer) OpenDirectTree(ctx context.Context, intent transfer.ReceiveIntent) (transfer.DirectTreeSession, error) {
	m.opens++
	if m.opens != 1 {
		return nil, errors.New("live output cannot reopen")
	}
	output, err := m.materializer.OpenDirectTree(ctx, intent)
	if err != nil {
		return nil, err
	}
	m.session = &singleFileSession{DirectTreeSession: output}
	return m.session, nil
}

type singleFileSession struct {
	transfer.DirectTreeSession
	starts int
}

func (s *singleFileSession) Capabilities() transfer.DirectTreeCapabilities {
	capabilities := s.DirectTreeSession.Capabilities()
	capabilities.Durability = 0
	return capabilities
}
func (s *singleFileSession) BeginFile(ctx context.Context, file transfer.MaterializationFile) (transfer.FileStart, error) {
	s.starts++
	if s.starts != 1 {
		return transfer.FileStart{}, errors.New("live file cannot reopen")
	}
	start, err := s.DirectTreeSession.BeginFile(ctx, file)
	if err != nil {
		return transfer.FileStart{}, err
	}
	transaction, initial, ok := start.Transaction()
	if !ok {
		return transfer.FileStart{}, errors.New("expected fresh file")
	}
	return transfer.NewFileTransactionStart(&transientFileTransaction{FileTransaction: transaction}, initial)
}

type transientFileTransaction struct{ transfer.FileTransaction }

func (f *transientFileTransaction) Checkpoint(ctx context.Context) (transfer.VerifiedDurableRanges, error) {
	checkpoint, err := f.FileTransaction.Checkpoint(ctx)
	if err != nil {
		return transfer.VerifiedDurableRanges{}, err
	}
	empty, _ := content.NewRangeSet(nil)
	return transfer.VerifyDurableRanges(f.Binding(), checkpoint.CheckpointGeneration(), empty)
}
func (f *transientFileTransaction) Commit(ctx context.Context) (transfer.FileSettlement, error) {
	if _, err := f.FileTransaction.Commit(ctx); err != nil {
		return transfer.FileSettlement{}, err
	}
	return transfer.NewTransientPublishedFileSettlement(f.Binding())
}

type rangeReaderFunc func(context.Context, content.LeaseID, content.FileRevisionDescriptor, content.Range, transfer.RangeSink) error

func (f rangeReaderFunc) ReadRange(ctx context.Context, l content.LeaseID, d content.FileRevisionDescriptor, r content.Range, s transfer.RangeSink) error {
	return f(ctx, l, d, r, s)
}

func TestContinuationRequiresWinningNetworkRetirementAndFreshIdentity(t *testing.T) {
	f := newFixture(t)
	runtime, _ := f.connect()
	called := false
	s, err := New(context.Background(), runtime, func(context.Context, *sessionruntime.ReceiverRuntime) (*sessionruntime.ReceiverRuntime, error) {
		called = true
		return runtime, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	runtime.Close()
	if err = s.recover(context.Background(), runtime); err == nil || called {
		t.Fatalf("caller close authorized recovery: %v called=%v", err, called)
	}
	var missingContext context.Context // Continuation requires an explicit receive-intent lifetime.
	if _, err = New(missingContext, runtime, s.replace); !errors.Is(err, ErrReplacement) {
		t.Fatal(err)
	}
	if _, err = New(t.Context(), runtime, nil); !errors.Is(err, ErrReplacement) {
		t.Fatal("missing replacement accepted", err)
	}
	token := s.Current()
	s.Close()
	if !s.Current().IsZero() {
		t.Fatal("closed receive intent retained generation authority")
	}
	change, err := s.WaitForChange(context.Background(), token)
	if err != nil || change.Kind() != revisionwait.GenerationLifetimeEnded {
		t.Fatalf("change=%v err=%v", change, err)
	}
}
