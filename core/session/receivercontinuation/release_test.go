package receivercontinuation

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/internal/testoutputroot"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

// Arming after prior operations complete isolates the release acknowledgement.
// Holding its real transport send proves the sender has already ended the lease
// while the receiver still awaits confirmation, without wall-clock timing.
type releaseResponseGate struct {
	framechannel.Channel
	armed   atomic.Bool
	entered chan struct{}
	proceed chan struct{}
}

func newReleaseResponseGate(channel framechannel.Channel) *releaseResponseGate {
	return &releaseResponseGate{Channel: channel, entered: make(chan struct{}), proceed: make(chan struct{})}
}

func (gate *releaseResponseGate) Send(ctx context.Context, frame framechannel.Frame) error {
	if gate.armed.CompareAndSwap(true, false) {
		close(gate.entered)
		select {
		case <-ctx.Done():
			return framechannel.RejectSend(ctx.Err())
		case <-gate.proceed:
		}
	}
	return gate.Channel.Send(ctx, frame)
}

func TestReleaseRevisionUsesWinningTermination(t *testing.T) {
	protocolFailure := errors.New("authenticated share identity rejected during release")
	for _, test := range []struct {
		name    string
		settle  func(*sessionruntime.ReceiverRuntime, *pipeChannel, *releaseResponseGate)
		wantErr bool
	}{
		{"acknowledged", func(_ *sessionruntime.ReceiverRuntime, _ *pipeChannel, gate *releaseResponseGate) {
			close(gate.proceed)
		}, false},
		{"paths_exhausted", func(_ *sessionruntime.ReceiverRuntime, wire *pipeChannel, _ *releaseResponseGate) {
			_ = wire.Close()
		}, false},
		{"protocol_rejected", func(runtime *sessionruntime.ReceiverRuntime, _ *pipeChannel, _ *releaseResponseGate) {
			runtime.RejectShareIdentity(protocolFailure)
		}, true},
		{"caller_closed", func(runtime *sessionruntime.ReceiverRuntime, _ *pipeChannel, _ *releaseResponseGate) {
			runtime.Close()
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			sender, wire := newPipe()
			gate := newReleaseResponseGate(sender)
			runtime := f.connectChannels(gate, wire)
			continuation, err := New(t.Context(), runtime, func(context.Context, *sessionruntime.ReceiverRuntime) (*sessionruntime.ReceiverRuntime, error) {
				t.Error("releasing a finished lease must not reconnect")
				return nil, ErrReplacement
			})
			if err != nil {
				t.Fatal(err)
			}
			defer continuation.Close()
			snapshot, release, err := continuation.AcquireDirectory(t.Context(), runtime.Descriptor().SyntheticRoot())
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			file, _ := snapshot.Pages()[0].Entries()[0].FileID()
			opened, err := continuation.OpenRevision(t.Context(), file)
			if err != nil {
				t.Fatal(err)
			}
			gate.armed.Store(true)
			released := make(chan error, 1)
			go func() { released <- continuation.ReleaseRevision(t.Context(), opened.Handle) }()
			select {
			case <-gate.entered:
			case err = <-released:
				t.Fatalf("release finished before its acknowledgement was withheld: %v", err)
			}
			test.settle(runtime, wire, gate)
			err = <-released
			if (err != nil) != test.wantErr {
				t.Fatalf("release error=%v, want error=%v", err, test.wantErr)
			}
			if test.name == "protocol_rejected" {
				var boundary *fault.BoundaryError
				if !errors.Is(err, protocolFailure) || !errors.As(err, &boundary) {
					t.Fatalf("release lost protocol failure: %v", err)
				}
				if code, ok := boundary.Fault().SessionCode(); !ok || code != fault.SessionProtocol {
					t.Fatalf("release changed protocol classification: %v", boundary.Fault())
				}
			}
			if runtime.PathsExhausted() != (test.name == "paths_exhausted") {
				t.Fatalf("incorrect termination authority: %v", runtime.Err())
			}
			if len(continuation.leases) != 0 {
				t.Fatal("release retained its local lease binding")
			}
		})
	}
}

type gatedRevisionRelease struct {
	*Session
	response *releaseResponseGate
}

func (revisions gatedRevisionRelease) ReleaseRevision(ctx context.Context, handle transfer.RevisionHandle) error {
	revisions.response.armed.Store(true)
	return revisions.Session.ReleaseRevision(ctx, handle)
}

func TestPublishedTransferCompletesAfterReleaseAcknowledgementLoss(t *testing.T) {
	f := newFixture(t)
	sender, wire := newPipe()
	gate := newReleaseResponseGate(sender)
	runtime := f.connectChannels(gate, wire)
	continuation, err := New(t.Context(), runtime, func(context.Context, *sessionruntime.ReceiverRuntime) (*sessionruntime.ReceiverRuntime, error) {
		t.Error("published output must not require a new connection for lease cleanup")
		return nil, ErrReplacement
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
	reservation, err := output.ReserveDirectTree(t.Context(), selection, receivecontract.NewCatalogRootDirectoryTree())
	if err != nil {
		t.Fatal(err)
	}
	intent, ok := reservation.ReceiveIntent()
	if !ok {
		t.Fatal("missing reservation")
	}
	id, _ := transfer.NewTransferJobID()
	job, err := transfer.NewTransferJob(transfer.TransferJobConfig{
		ReceiveIntent: intent, JobID: id, Session: continuation,
		Catalog: continuation, Revisions: gatedRevisionRelease{Session: continuation, response: gate},
		Blocks: continuation, Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	completed := make(chan transfer.JobResult, 1)
	go func() { completed <- job.Run(t.Context()) }()
	select {
	case <-gate.entered:
	case result := <-completed:
		t.Fatalf("job finished before its release acknowledgement was withheld: %+v", result)
	}
	destination, _ := intent.MaterializationPlan().DestinationReservation()
	written, err := os.ReadFile(filepath.Join(out.RootPath, destination.PhysicalName(), "source.bin"))
	// Close before asserting so a failed assertion cannot strand the job or its
	// output authority behind the deliberately withheld acknowledgement.
	_ = wire.Close()
	result := <-completed
	if err != nil || !bytes.Equal(written, f.payload) {
		t.Fatalf("file was not published before disconnect: bytes=%d err=%v", len(written), err)
	}
	if result.Outcome != transfer.DirectTreeOutcomeSuccess || result.Settlement.Kind() != transfer.DirectTreeSettlementSuccess ||
		result.TerminationCause != nil || result.SettlementFailure != nil || len(result.Files) != 0 {
		t.Fatalf("published download did not complete: outcome=%v settlement=%v termination=%v failure=%v files=%v",
			result.Outcome, result.Settlement.Kind(), result.TerminationCause, result.SettlementFailure, result.Files)
	}
	if result.Progress.PublishedFiles != 1 || result.Progress.PublishedBytes != uint64(len(f.payload)) {
		t.Fatalf("published progress changed: %+v", result.Progress)
	}
	if len(continuation.leases) != 0 {
		t.Fatal("completed job retained its local lease binding")
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	resume, err := osfs.NewFilesystemResumeStateAuthority(osfs.FilesystemResumeRoot{RootPath: out.RootPath})
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := resume.ListResumeState(t.Context())
	if err != nil || inventory.Status() != osfs.ResumeStateListReady || inventory.UnknownEntries() || len(inventory.Summaries()) != 0 {
		t.Fatalf("completed download retained recovery state: inventory=%+v err=%v", inventory, err)
	}
}
