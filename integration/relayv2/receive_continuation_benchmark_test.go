package relayv2_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/receivercontinuation"
	"github.com/windshare/windshare/core/session/receiversession"
	"github.com/windshare/windshare/core/transfer"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/core/transfer/revisionwait"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/internal/testoutputroot"
	"github.com/windshare/windshare/internal/testscenario"
	"github.com/windshare/windshare/osfs"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

const (
	receiveEvidenceFileName = "payload.bin"
	receiveEvidenceFileSize = 16 << 20
)

type receiveEvidenceScenario string

const (
	evidenceUninterrupted receiveEvidenceScenario = "uninterrupted"
	evidenceReopened      receiveEvidenceScenario = "reopened"
)

// The disk fixture's identity must change on reopen even on filesystems that
// can ordinarily prove catalog continuity. Its metadata and bytes stay fixed.
type handleBoundEvidenceSource struct{ liveshare.FileSource }

func (handleBoundEvidenceSource) RevisionContinuity(catalog.NodeRecord) (content.RevisionContinuity, error) {
	return content.OpenHandleRevisionContinuity, nil
}

func BenchmarkNativeReceiveContinuation(b *testing.B) {
	sourcePath := filepath.Join(b.TempDir(), receiveEvidenceFileName)
	trace, relays := startNativeRelays(b, 1)
	relayURL := relays[0]
	payload := make([]byte, receiveEvidenceFileSize)
	generator := rand.NewChaCha8([32]byte{})
	if _, err := generator.Read(payload); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, payload, 0o600); err != nil {
		b.Fatal(err)
	}
	capability := shareReceiveEvidenceFile(b, trace, sourcePath, relayURL)
	defer clear(capability.ReadSecret)
	shareIDBytes, err := base64.RawURLEncoding.DecodeString(capability.ShareID)
	if err != nil {
		b.Fatal(err)
	}
	shareID, err := v2.ShareIDFromBytes(shareIDBytes)
	if err != nil {
		b.Fatal(err)
	}
	relay := relayv2.ReceiverConfig{RelayBaseURL: relayURL, ShareID: shareID}
	for _, scenario := range []receiveEvidenceScenario{evidenceUninterrupted, evidenceReopened} {
		b.Run(string(scenario), func(b *testing.B) {
			benchmarkReceiveEvidence(b, scenario, relay, capability, payload)
		})
	}
	if !b.Failed() {
		trace.RequireSuccess(b)
	}
}

func shareReceiveEvidenceFile(b *testing.B, trace *testscenario.Trace, sourcePath, relayURL string) link.Link {
	b.Helper()
	application, err := engine.New(engine.Config{})
	if err != nil {
		b.Fatal(err)
	}
	trace.RequireCleanup(b, "receive evidence sender", application.Close)
	ctx, cancel := context.WithTimeout(b.Context(), recoveryIntegrationTimeout)
	defer cancel()
	sender, err := application.StartShare(b.Context(), engine.ShareRequest{
		RelayIntent: relayprofile.UseExplicit([]relayprofile.Candidate{{PublicURL: relayURL}}),
		Source: liveshare.FileSourceFactoryFunc(func(ctx context.Context, source liveshare.FileSourceContext) (liveshare.FileSource, error) {
			files, err := osfs.NewSelectedFileSource(ctx, osfs.SelectedCatalogSourceConfig{
				Paths: []string{sourcePath}, SyntheticRoot: source.SyntheticRoot,
				Identities: osfs.CatalogIdentitySourceFunc(source.NewIdentity), Diagnostics: source.Diagnostics,
				Omissions: source.Omissions,
			})
			if err != nil {
				return nil, err
			}
			return handleBoundEvidenceSource{files}, nil
		}),
	})
	if err != nil {
		b.Fatal(err)
	}
	ready, err := sender.Ready(ctx)
	if err != nil {
		b.Fatal(err)
	}
	return ready.Capability
}

func benchmarkReceiveEvidence(
	b *testing.B, scenario receiveEvidenceScenario, relay relayv2.ReceiverConfig, capability link.Link, payload []byte,
) {
	b.StopTimer()
	capacity := testReceiveCapacity(b)
	b.ResetTimer()
	var resumeTime time.Duration
	var reusedBytes, redownloadedBytes uint64
	for range b.N {
		recovery := runReceiveEvidenceSample(b, scenario, relay, capability, capacity, payload)
		resumeTime += recovery.timeToResume
		reusedBytes += recovery.readmission.Reused
		redownloadedBytes += recovery.redownloadedBytes
	}
	if scenario == evidenceUninterrupted {
		b.SetBytes(int64(len(payload)))
		return
	}
	b.ReportMetric(float64(resumeTime)/float64(time.Millisecond)/float64(b.N), "resume-ms/op")
	b.ReportMetric(float64(reusedBytes)/float64(b.N), "reused-B/op")
	b.ReportMetric(float64(redownloadedBytes)/float64(b.N), "redownloaded-B/op")
}

func runReceiveEvidenceSample(
	b *testing.B, scenario receiveEvidenceScenario, relay relayv2.ReceiverConfig, capability link.Link,
	capacity *liveshare.ReceiveCapacity, payload []byte,
) receiveRecoveryEvidence {
	b.Helper()
	ctx, cancel := context.WithTimeout(b.Context(), recoveryIntegrationTimeout)
	defer cancel()
	initial, err := relayv2.DialReceiver(ctx, relay)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = initial.Close() }()
	receiver, err := liveshare.PrepareReceiver(liveshare.ReceiverConfig{
		Capacity: capacity, Capability: capability, DescriptorObject: initial.Descriptor(),
		ContentRoutePolicy: transferlanes.ContentRouteRelayOnly,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer receiver.Close()
	runtime, err := receiver.Connect(ctx, initial.Channel(), transferlanes.RouteRelay)
	if err != nil {
		b.Fatal(err)
	}
	var replacement *relayv2.ReceiverConnection
	defer func() { _ = replacement.Close() }()
	continuation, err := receivercontinuation.New(ctx, runtime, func(ctx context.Context, _ *receiversession.Runtime) (*receiversession.Runtime, error) {
		if scenario != evidenceReopened || replacement != nil {
			return nil, errors.New("unexpected receiver replacement in the receive benchmark")
		}
		connection, err := relayv2.DialReceiver(ctx, relay)
		if err != nil {
			return nil, err
		}
		replacement = connection
		return receiver.Connect(ctx, connection.Channel(), transferlanes.RouteRelay)
	})
	if err != nil {
		b.Fatal(err)
	}
	defer continuation.Close()

	root := testoutputroot.New(b)
	output, err := osfs.NewFilesystemOutputAuthority(osfs.FilesystemOutputAuthorityConfig{
		RootPath: root.RootPath, CreateRoot: root.CreateRoot,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		if err := output.Close(); err != nil {
			b.Error(err)
		}
	}()
	intent, err := reserveReceiveEvidenceOutput(ctx, runtime.Descriptor(), output)
	if err != nil {
		b.Fatal(err)
	}
	id, err := transfer.NewTransferJobID()
	if err != nil {
		b.Fatal(err)
	}
	config := transfer.TransferJobConfig{
		ReceiveIntent: intent, JobID: id, Session: continuation, Catalog: continuation,
		Revisions: continuation, Blocks: continuation, Materializer: output,
		RevisionWait: &revisionwait.Config{GenerationFence: continuation},
	}
	var recovery receiveRecoveryEvidence
	if scenario == evidenceReopened {
		reader := &reopeningEvidenceReader{
			Session: continuation, initial: initial, recovery: &recovery,
		}
		config.Blocks = reader
		config.Tracer = transfer.TransferLifecycleTraceFunc(recovery.observeReadmission)
	}
	job, err := transfer.NewTransferJob(config)
	if err != nil {
		b.Fatal(err)
	}
	b.StartTimer()
	result := job.Run(ctx)
	b.StopTimer()

	if result.Outcome != transfer.DirectTreeOutcomeSuccess || result.SucceededFiles != 1 ||
		!result.Progress.CountersExact || result.Progress.VerifiedBytes != uint64(len(payload)) {
		b.Fatalf("receive outcome=%d progress=%+v termination=%v settlement=%v files=%+v",
			result.Outcome, result.Progress, result.TerminationCause, result.SettlementFailure, result.Files)
	}
	destination, _ := intent.MaterializationPlan().DestinationReservation()
	written, err := os.ReadFile(filepath.Join(root.RootPath, destination.PhysicalName(), receiveEvidenceFileName))
	if err != nil || !bytes.Equal(written, payload) {
		b.Fatalf("published file differs from the fixture: got %d bytes, error=%v", len(written), err)
	}
	if scenario == evidenceReopened {
		if err := recovery.validate(); err != nil {
			b.Fatal(err)
		}
	}
	return recovery
}

func reserveReceiveEvidenceOutput(ctx context.Context, descriptor catalog.ShareDescriptor, output *osfs.FilesystemOutputAuthority) (transfer.ReceiveIntent, error) {
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		return transfer.ReceiveIntent{}, err
	}
	selection, err := transfer.NewSelectionSpec(descriptor.ShareInstance(), descriptor.SyntheticRoot(), rules)
	if err != nil {
		return transfer.ReceiveIntent{}, err
	}
	reservation, err := output.ReserveDirectTree(ctx, selection, receivecontract.NewCatalogRootDirectoryTree())
	if err != nil {
		return transfer.ReceiveIntent{}, err
	}
	intent, ok := reservation.ReceiveIntent()
	if !ok {
		return transfer.ReceiveIntent{}, errors.New("missing native receive benchmark output reservation")
	}
	return intent, nil
}
