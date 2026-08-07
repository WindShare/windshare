package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"math"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer"
)

func TestAppCommandSurfaceReportsActionableFailures(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStderr string
	}{
		{name: "missing command", wantCode: ExitUsage, wantStderr: "Usage:"},
		{name: "help", args: []string{"help"}, wantCode: ExitOK, wantStderr: "Usage:"},
		{name: "unknown command", args: []string{"unknown"}, wantCode: ExitUsage, wantStderr: "unknown command"},
		{
			name:       "malformed share flag",
			args:       []string{"share", "--definitely-unknown"},
			wantCode:   ExitUsage,
			wantStderr: "flag provided but not defined",
		},
		{
			name:       "missing get link",
			args:       []string{"get"},
			wantCode:   ExitUsage,
			wantStderr: "exactly one link argument is required",
		},
		{
			name:       "malformed get flag",
			args:       []string{"get", "--definitely-unknown"},
			wantCode:   ExitUsage,
			wantStderr: "flag provided but not defined",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, _, stderr := newSemanticTestApp(strings.NewReader(""))
			if code := app.Run(context.Background(), test.args); code != test.wantCode {
				t.Fatalf("exit=%d want=%d stderr=%q", code, test.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), test.wantStderr) {
				t.Fatalf("stderr=%q does not contain %q", stderr.String(), test.wantStderr)
			}
		})
	}
}

func TestShareRequestValidationPreservesSuiteBoundaries(t *testing.T) {
	t.Run("valid explicit configuration", func(t *testing.T) {
		app, _, _ := newSemanticTestApp(strings.NewReader(""))
		request, code := app.parseShareRequest([]string{
			"root", "--relay", "wss://relay.example", "--front-url", "https://app.example",
			"--block-size", "65536", "--split-key",
		})
		if code != ExitOK {
			t.Fatalf("exit=%d", code)
		}
		if !reflect.DeepEqual(request.paths, []string{"root"}) ||
			request.relayURL != "wss://relay.example" ||
			request.frontURL != "https://app.example" ||
			request.chunkSize != 65536 ||
			!request.splitKey {
			t.Fatalf("request=%+v", request)
		}
	})

	for _, test := range []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{name: "missing roots", wantStderr: "at least one path"},
		{name: "empty relay", args: []string{"root", "--relay", ""}, wantStderr: "relay URL"},
		{name: "empty frontend", args: []string{"root", "--front-url", ""}, wantStderr: "frontend URL"},
		{
			name:       "negative block size",
			args:       []string{"root", "--block-size", "-1"},
			wantStderr: "outside the suite-02 range",
		},
		{
			name: "block size exceeds wire width",
			args: []string{
				"root", "--block-size", strconv.FormatUint(uint64(math.MaxUint32)+1, 10),
			},
			wantStderr: "outside the suite-02 range",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, _, stderr := newSemanticTestApp(strings.NewReader(""))
			if _, code := app.parseShareRequest(test.args); code != ExitUsage {
				t.Fatalf("exit=%d want=%d", code, ExitUsage)
			}
			if !strings.Contains(stderr.String(), test.wantStderr) {
				t.Fatalf("stderr=%q does not contain %q", stderr.String(), test.wantStderr)
			}
		})
	}

	t.Run("missing source fails before relay registration", func(t *testing.T) {
		app, _, stderr := newSemanticTestApp(strings.NewReader(""))
		missing := filepath.Join(t.TempDir(), "missing")
		if code := app.Run(context.Background(), []string{"share", missing}); code != ExitUsage {
			t.Fatalf("exit=%d want=%d stderr=%q", code, ExitUsage, stderr.String())
		}
		if !strings.Contains(stderr.String(), "prepare selected roots") {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})
}

func TestGetCapabilityInputStaysLocalAndUnambiguous(t *testing.T) {
	capability := newSemanticCapability(t, "wss://relay.example")
	bare, keyString, err := capability.SplitURL("https://app.example")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("explicit key", func(t *testing.T) {
		app, _, stderr := newSemanticTestApp(strings.NewReader("ignored"))
		resolved, err := app.resolveLink(bare, keyString)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(resolved, capability) {
			t.Fatalf("resolved=%+v want=%+v", resolved, capability)
		}
		if stderr.Len() != 0 {
			t.Fatalf("explicit key unexpectedly prompted: %q", stderr.String())
		}
	})

	t.Run("interactive key", func(t *testing.T) {
		app, _, stderr := newSemanticTestApp(strings.NewReader(keyString + "\n"))
		resolved, err := app.resolveLink(bare, "")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(resolved, capability) {
			t.Fatalf("resolved=%+v want=%+v", resolved, capability)
		}
		if !strings.Contains(stderr.String(), "enter the key string") {
			t.Fatalf("prompt=%q", stderr.String())
		}
	})

	t.Run("empty entered key", func(t *testing.T) {
		app, _, _ := newSemanticTestApp(strings.NewReader("\n"))
		if _, err := app.resolveLink(bare, ""); err == nil || err.Error() != missingCapabilityKeyDiagnostic {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("key input failure", func(t *testing.T) {
		app, _, _ := newSemanticTestApp(strings.NewReader(""))
		if _, err := app.resolveLink(bare, ""); !errors.Is(err, io.EOF) {
			t.Fatalf("error=%v want wrapped EOF", err)
		}
	})

	t.Run("invalid link has a stable typed usage diagnostic", func(t *testing.T) {
		app, _, stderr := newSemanticTestApp(strings.NewReader(""))
		if _, code := app.parseGetRequest([]string{"not-a-link"}); code != ExitUsage {
			t.Fatalf("exit=%d want=%d", code, ExitUsage)
		}
		if stderr.String() != "get: "+invalidCapabilityDiagnostic+"\n" {
			t.Fatalf("stderr=%q", stderr.String())
		}
		_, err := app.resolveLink("not-a-link", "")
		var typed *capabilityInputError
		if !errors.As(err, &typed) || typed.kind != capabilityInputInvalid {
			t.Fatalf("error=%#v", err)
		}
	})

	t.Run("relay address is mandatory", func(t *testing.T) {
		withoutRelay := newSemanticCapability(t)
		full, err := withoutRelay.URL("https://app.example")
		if err != nil {
			t.Fatal(err)
		}
		app, _, stderr := newSemanticTestApp(strings.NewReader(""))
		if _, code := app.parseGetRequest([]string{full}); code != ExitUsage {
			t.Fatalf("exit=%d want=%d", code, ExitUsage)
		}
		if !strings.Contains(stderr.String(), "link has no relay address") {
			t.Fatalf("stderr=%q", stderr.String())
		}
	})
}

func TestReportTransferResultPreservesExitCodePrecedence(t *testing.T) {
	closed, err := transfer.NewJobSettlement(transfer.JobClosed)
	if err != nil {
		t.Fatal(err)
	}
	paused, err := transfer.NewJobSettlement(transfer.JobPaused)
	if err != nil {
		t.Fatal(err)
	}
	needsAttention, err := transfer.NewJobSettlement(transfer.JobPausedNeedsAttention)
	if err != nil {
		t.Fatal(err)
	}
	genericDirectoryFailure := transfer.DirectoryJobFailure{
		Path: "ordinary-directory", Stage: transfer.FailureDirectoryDiscovery, Cause: errors.New("directory unavailable"),
	}
	genericFileFailure := transfer.FileJobFailure{
		Path: "ordinary-file", Stage: transfer.FailureBlockTransfer, Cause: errors.New("block unavailable"),
	}
	tests := []struct {
		name         string
		result       transfer.JobResult
		cancel       bool
		admissionErr error
		wantCode     int
		wantLogs     []string
	}{
		{
			name: "success",
			result: transfer.JobResult{
				Outcome: transfer.JobSucceeded, Settlement: closed, SucceededFiles: 2,
				Measure: transfer.SelectionMeasure{
					DiscoveredFiles: 2, DiscoveredBytes: 8, CompletedFiles: 2, CompletedBytes: 8,
					Discovery: transfer.DiscoveryComplete, DiscoveryTerminalSuccess: true,
				},
			},
			wantCode: ExitOK, wantLogs: []string{"completed 2 file(s), 8 byte(s)"},
		},
		{
			name: "isolated ordinary failures",
			result: transfer.JobResult{
				Outcome: transfer.JobCompletedWithErrors, Settlement: closed,
				Directories: []transfer.DirectoryJobFailure{genericDirectoryFailure},
				Files:       []transfer.FileJobFailure{genericFileFailure},
			},
			wantCode: ExitFailure,
			wantLogs: []string{"directory \"ordinary-directory\" failed", "file \"ordinary-file\" failed"},
		},
		{
			name: "directory drift dominates isolated completion",
			result: transfer.JobResult{
				Outcome: transfer.JobCompletedWithErrors, Settlement: closed,
				SourceDriftFailure: catalog.ErrDirectoryStale,
				Directories: []transfer.DirectoryJobFailure{{
					Path: "stale-directory", Stage: transfer.FailureDirectoryDiscovery, Cause: catalog.ErrDirectoryStale,
				}},
			},
			wantCode: ExitDrift, wantLogs: []string{"stale-directory"},
		},
		{
			name: "file drift dominates isolated completion",
			result: transfer.JobResult{
				Outcome: transfer.JobCompletedWithErrors, Settlement: closed,
				SourceDriftFailure: content.ErrSourceDrift,
				Files: []transfer.FileJobFailure{{
					Path: "stale-file", Stage: transfer.FailureRevisionOpen, Cause: content.ErrSourceDrift,
				}},
			},
			wantCode: ExitDrift, wantLogs: []string{"stale-file"},
		},
		{
			name: "omitted drift remains exact",
			result: transfer.JobResult{
				Outcome: transfer.JobCompletedWithErrors, Settlement: closed,
				OmittedFileFailures: transfer.MaximumRetainedJobFailures,
				SourceDriftFailure:  content.ErrRevisionDrift,
			},
			wantCode: ExitDrift,
		},
		{
			name:     "caller cancellation dominates generic termination",
			result:   transfer.JobResult{Outcome: transfer.JobPausedOutcome, Settlement: paused, TerminationCause: errors.New("transfer stopped")},
			cancel:   true,
			wantCode: ExitFailure, wantLogs: []string{"interrupted"},
		},
		{
			name: "session failure dominates racing caller cancellation",
			result: transfer.JobResult{
				Outcome: transfer.JobPausedOutcome, Settlement: paused,
				TerminationCause: transfer.NewSessionFailure(errors.Join(
					context.Canceled,
					errors.New("authenticated runtime failed"),
				)),
			},
			cancel: true, wantCode: ExitNetwork, wantLogs: []string{"transfer stopped"},
		},
		{
			name: "relay admission failure remains network-visible",
			result: transfer.JobResult{
				Outcome: transfer.JobPausedOutcome, Settlement: paused, TerminationCause: errors.New("transfer stopped"),
			},
			admissionErr: errors.New("resume suspended relay failed"),
			wantCode:     ExitNetwork, wantLogs: []string{"resume suspended relay failed"},
		},
		{
			name: "missing explicit selection is usage",
			result: transfer.JobResult{
				Outcome: transfer.JobCompletedWithErrors, Settlement: closed,
				Measure: transfer.SelectionMeasure{
					Discovery: transfer.DiscoveryComplete, DiscoveryTerminalSuccess: true,
				},
				SelectionResolutionFailure: errors.Join(transfer.ErrSelectionTargetMissing, errors.New("path: missing")),
			},
			wantCode: ExitUsage, wantLogs: []string{"selection target was not found"},
		},
		{
			name: "proven missing selection completes as usage error",
			result: transfer.JobResult{
				Outcome: transfer.JobCompletedWithErrors, Settlement: closed,
				Measure: transfer.SelectionMeasure{
					Discovery: transfer.DiscoveryComplete, DiscoveryTerminalSuccess: true,
				},
				SelectionResolutionFailure: errors.Join(transfer.ErrSelectionTargetMissing, errors.New("path: missing")),
				OmittedDirectoryFailures:   transfer.MaximumRetainedJobFailures,
			},
			wantCode: ExitUsage, wantLogs: []string{"selection target was not found"},
		},
		{
			name: "partial discovery cannot claim an explicit selection is missing",
			result: transfer.JobResult{
				Outcome:                    transfer.JobCompletedWithErrors,
				Settlement:                 closed,
				Measure:                    transfer.SelectionMeasure{Discovery: transfer.DiscoveryOpen},
				SelectionResolutionFailure: transfer.ErrSelectionTargetMissing,
			},
			wantCode: ExitFailure, wantLogs: []string{"selection target remains unknown/partial"},
		},
		{
			name: "paused drift precedes terminal transport inspection",
			result: transfer.JobResult{
				Outcome: transfer.JobPausedOutcome, Settlement: paused,
				TerminationCause: content.ErrRevisionStale, SourceDriftFailure: content.ErrRevisionStale,
			},
			wantCode: ExitDrift,
		},
		{
			name: "settlement failure remains local and visible",
			result: transfer.JobResult{
				Outcome: transfer.JobPausedOutcome, SettlementFailure: errors.New("checkpoint install failed"),
				Files: []transfer.FileJobFailure{{
					Path: "partial.bin", Stage: transfer.FailureFileOutput,
					Cause: errors.New("write failed"), SettlementFailure: errors.New("file pause failed"),
					LeaseReleaseFailure: errors.New("lease release failed"),
				}},
			},
			wantCode: ExitFailure,
			wantLogs: []string{"durable output settlement failed", "file pause failed", "lease release failed"},
		},
		{
			name: "successful files with retained attention are not silent success",
			result: transfer.JobResult{
				Outcome: transfer.JobSucceeded, Settlement: needsAttention, SucceededFiles: 1,
				Measure: transfer.SelectionMeasure{
					DiscoveredFiles: 1, DiscoveredBytes: 4, CompletedFiles: 1, CompletedBytes: 4,
					Discovery: transfer.DiscoveryComplete, DiscoveryTerminalSuccess: true,
				},
			},
			wantCode: ExitFailure,
			wantLogs: []string{"needs attention", "completed 1 file(s), 4 byte(s)"},
		},
		{
			name: "success cannot hide incomplete output",
			result: transfer.JobResult{
				Outcome: transfer.JobSucceeded, Settlement: closed, SucceededFiles: 1,
				Measure: transfer.SelectionMeasure{
					DiscoveredFiles: 2, DiscoveredBytes: 8, CompletedFiles: 1, CompletedBytes: 4,
					Discovery: transfer.DiscoveryComplete, DiscoveryTerminalSuccess: true,
				},
			},
			wantCode: ExitFailure, wantLogs: []string{"success with incomplete output"},
		},
		{
			name: "success outcome cannot hide settlement failure",
			result: transfer.JobResult{
				Outcome: transfer.JobSucceeded, Settlement: closed,
				SettlementFailure: errors.New("terminal cleanup failed"),
			},
			wantCode: ExitFailure,
			wantLogs: []string{"terminal cleanup failed", "success with terminal failure state"},
		},
		{
			name: "invalid outcome",
			result: transfer.JobResult{
				Outcome: transfer.JobOutcome(255),
			},
			wantCode: ExitFailure, wantLogs: []string{"invalid outcome"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			if test.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			app, _, stderr := newSemanticTestApp(strings.NewReader(""))
			// The tested outcomes return before terminal runtime inspection; nil
			// values make that boundary explicit and would panic on precedence drift.
			if code := app.reportTransferResultWithAdmission(
				ctx, nil, nil, test.result, test.admissionErr,
			); code != test.wantCode {
				t.Fatalf("exit=%d want=%d stderr=%q", code, test.wantCode, stderr.String())
			}
			for _, expected := range test.wantLogs {
				if !strings.Contains(stderr.String(), expected) {
					t.Fatalf("stderr=%q does not contain %q", stderr.String(), expected)
				}
			}
		})
	}
}

func TestFilesystemOutputTraceKeepsTheHappyPathQuietAndFailuresStructured(t *testing.T) {
	app, _, stderr := newSemanticTestApp(strings.NewReader(""))
	var session transfer.OutputSessionID
	session[0] = 2
	var locator transfer.OutputLocatorDigest
	locator[0] = 3
	var object transfer.OutputObjectIdentity
	object[0] = 4

	for _, event := range []osfs.FilesystemOutputTrace{
		{Operation: osfs.TraceFilesystemCertified, Certification: osfs.FilesystemOutputCertificationWindowsNTFSProcessRestart},
		{Operation: osfs.TraceSessionOpened, SessionID: session},
	} {
		app.traceFilesystemOutput(event)
	}
	quietAt := stderr.Len()
	for _, event := range []osfs.FilesystemOutputTrace{
		{Operation: osfs.TraceFileRecoveryDecision, RecoveryAction: osfs.FilesystemOutputRecoveryRetryObjectCreation},
		{Operation: osfs.TraceFileSettlement, FileSettlement: transfer.FilePublished},
		{Operation: osfs.TraceAncestryValidation, AncestryDecision: osfs.FilesystemOutputAncestryMatched},
		{Operation: osfs.TraceNativeLock, NativeLockMilestone: osfs.FilesystemOutputNativeLockAcquired},
		{Operation: osfs.TraceFeatureProbeCompleted},
	} {
		event.SessionID, event.LocatorDigest, event.OutputObjectID = session, locator, object
		app.traceFilesystemOutput(event)
	}
	if stderr.Len() != quietAt {
		t.Fatalf("successful high-volume trace was logged: %q", stderr.String())
	}

	noisy := []osfs.FilesystemOutputTrace{
		{Operation: osfs.TraceFileRecoveryDecision, RecoveryAction: osfs.FilesystemOutputRecoveryResumeContent},
		{Operation: osfs.TraceFileRecoveryDecision, RecoveryAction: osfs.FilesystemOutputRecoveryInstallQuarantine, QuarantineReason: transfer.QuarantineOwnershipMismatch},
		{Operation: osfs.TraceFileRecoveryDecision, RecoveryAction: osfs.FilesystemOutputRecoveryHoldQuarantine, QuarantineReason: transfer.QuarantineStateCorrupt},
		{Operation: osfs.TraceFileSettlement, FileSettlement: transfer.FileQuarantined, QuarantineReason: transfer.QuarantineOwnershipMismatch},
		{Operation: osfs.TraceSessionSettlement, JobSettlement: transfer.JobPausedNeedsAttention},
		{Operation: osfs.TraceStateInstallCutAdopted, StateInstallStage: osfs.FilesystemOutputStateReplace, StateGeneration: 7, MutationReportedFailure: true},
		{Operation: osfs.TraceAncestryValidation, AncestryBoundary: osfs.FilesystemOutputAncestryRestart, AncestryDecision: osfs.FilesystemOutputAncestryMismatch, AncestryClaimCount: 2, Failed: true},
		{Operation: osfs.TraceNativeLock, NativeLockScope: osfs.FilesystemOutputNativeLockSession, NativeLockMilestone: osfs.FilesystemOutputNativeLockContended},
		{Operation: osfs.TraceFeatureProbeCompleted, FailureScope: transfer.OutputFaultRoot, FailureCode: transfer.OutputFaultUnsupportedFilesystem, Failed: true},
	}
	for _, event := range noisy {
		event.SessionID, event.LocatorDigest, event.OutputObjectID = session, locator, object
		app.traceFilesystemOutput(event)
	}
	for _, expected := range []string{
		"operation=" + strconv.Itoa(int(osfs.TraceFilesystemCertified)),
		"operation=" + strconv.Itoa(int(osfs.TraceSessionOpened)),
		"recovery_action=\"resume-content\"", "file_settlement=quarantined",
		"job_settlement=paused-needs-attention",
		"state_install_stage=" + strconv.Itoa(int(osfs.FilesystemOutputStateReplace)) + " state_generation=7",
		"ancestry_boundary=" + strconv.Itoa(int(osfs.FilesystemOutputAncestryRestart)) +
			" ancestry_decision=" + strconv.Itoa(int(osfs.FilesystemOutputAncestryMismatch)) + " ancestry_claims=2",
		"native_lock_scope=" + strconv.Itoa(int(osfs.FilesystemOutputNativeLockSession)) +
			" native_lock_milestone=" + strconv.Itoa(int(osfs.FilesystemOutputNativeLockContended)),
		"failure_scope=3 failure_code=5", "failed=true",
	} {
		if !strings.Contains(stderr.String(), expected) {
			t.Fatalf("trace %q does not contain %q", stderr.String(), expected)
		}
	}

	if got := outputTraceIdentity(nil); got != "-" {
		t.Fatalf("zero trace identity=%q", got)
	}
	if got := outputTraceIdentity([]byte{1}); got != "01" {
		t.Fatalf("short trace identity=%q", got)
	}
	if got := outputTraceIdentity(bytes.Repeat([]byte{2}, outputTraceIdentityPrefixBytes+1)); len(got) != 2*outputTraceIdentityPrefixBytes {
		t.Fatalf("bounded trace identity=%q", got)
	}
	for kind, expected := range map[transfer.FileSettlementKind]string{
		transfer.FilePublished: "published", transfer.FilePaused: "paused", transfer.FileRetired: "retired",
		transfer.FileCollision: "collision", transfer.FilePublishBlocked: "publish-blocked", transfer.FileQuarantined: "quarantined",
		0: "none",
	} {
		if got := fileSettlementName(kind); got != expected {
			t.Fatalf("file settlement %d=%q want=%q", kind, got, expected)
		}
	}
	for kind, expected := range map[transfer.JobSettlementKind]string{
		transfer.JobClosed: "closed", transfer.JobPaused: "paused",
		transfer.JobPausedNeedsAttention: "paused-needs-attention", 0: "none",
	} {
		if got := jobSettlementName(kind); got != expected {
			t.Fatalf("job settlement %d=%q want=%q", kind, got, expected)
		}
	}
}

func TestTransferLifecycleTraceProjectsTypedSelectionDecision(t *testing.T) {
	app, _, stderr := newSemanticTestApp(strings.NewReader(""))
	app.traceTransferLifecycle(transfer.TransferLifecycleTrace{
		Stage:         transfer.TransferFileEnqueued,
		FileSelection: transfer.FileSelectionCatalogPathTarget,
	})
	if !strings.Contains(stderr.String(), "file_selection=3") {
		t.Fatalf("transfer trace=%q", stderr.String())
	}
}

func TestRelayContentAdmissionRejectsBrokenDependenciesWithoutStrandingContent(t *testing.T) {
	t0 := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	validClock := &semanticAdmissionClock{now: t0, timer: newSemanticAdmissionTimer()}
	validRelay := &semanticAdmissionSuspension{}

	for _, test := range []struct {
		name  string
		t0    time.Time
		clock receiverAdmissionClock
		relay receiverContentSuspension
	}{
		{name: "zero T0", clock: validClock, relay: validRelay},
		{name: "nil clock", t0: t0, relay: validRelay},
		{name: "nil relay suspension", t0: t0, clock: validClock},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := newRelayContentAdmission(test.t0, test.clock, test.relay); !errors.Is(err, ErrInvalidReceiverAdmission) {
				t.Fatalf("error=%v want=%v", err, ErrInvalidReceiverAdmission)
			}
		})
	}

	t.Run("expired absolute deadline clamps to immediate", func(t *testing.T) {
		clock := &semanticAdmissionClock{
			now: t0.Add(receiverRelayAdmissionWindow + time.Second), timer: newSemanticAdmissionTimer(),
		}
		relay := &semanticAdmissionSuspension{}
		admission, err := newRelayContentAdmission(t0, clock, relay)
		if err != nil {
			t.Fatal(err)
		}
		delays := clock.recordedDelays()
		if !reflect.DeepEqual(delays, []time.Duration{0}) {
			t.Fatalf("delays=%v want=[0]", delays)
		}
		admission.Close()
	})

	t.Run("broken timer rolls suspension back", func(t *testing.T) {
		resumeErr := errors.New("rollback failed")
		clock := &semanticAdmissionClock{now: t0}
		relay := &semanticAdmissionSuspension{resumeErr: resumeErr}
		if _, err := newRelayContentAdmission(t0, clock, relay); !errors.Is(err, ErrInvalidReceiverAdmission) || !errors.Is(err, resumeErr) {
			t.Fatalf("error=%v must retain setup and rollback failures", err)
		}
		if resumes := relay.resumeCount(); resumes != 1 {
			t.Fatalf("resume calls=%d want=1", resumes)
		}
	})

	t.Run("broken timer contains rollback panic", func(t *testing.T) {
		clock := &semanticAdmissionClock{now: t0}
		relay := receiverContentSuspensionFunc(func() error { panic("rollback panic") })
		if _, err := newRelayContentAdmission(t0, clock, relay); !errors.Is(err, ErrInvalidReceiverAdmission) ||
			!errors.Is(err, errReceiverAdmissionResumePanics) {
			t.Fatalf("panic rollback error=%v", err)
		}
	})

	t.Run("unknown policy signals fail closed", func(t *testing.T) {
		clock := &semanticAdmissionClock{now: t0, timer: newSemanticAdmissionTimer()}
		relay := &semanticAdmissionSuspension{}
		admission, err := newRelayContentAdmission(t0, clock, relay)
		if err != nil {
			t.Fatal(err)
		}
		if err := admission.ObserveSelection(transfer.SelectionClass(255)); !errors.Is(err, ErrInvalidReceiverAdmission) {
			t.Fatalf("selection error=%v", err)
		}
		if err := admission.ObservePeer(receiverPeerSignal(255)); !errors.Is(err, ErrInvalidReceiverAdmission) {
			t.Fatalf("peer error=%v", err)
		}
		if resumes := relay.resumeCount(); resumes != 0 {
			t.Fatalf("invalid signals resumed content %d time(s)", resumes)
		}
		admission.Close()
	})

	t.Run("injected clock is authoritative", func(t *testing.T) {
		clock := &semanticAdmissionClock{now: t0, timer: newSemanticAdmissionTimer()}
		app := &App{receiverClock: clock}
		if got := app.admissionClock(); got != clock {
			t.Fatalf("clock=%T want injected clock", got)
		}
	})

	t.Run("nil close is safe", func(t *testing.T) {
		var admission *relayContentAdmission
		admission.Close()
	})
}

func newSemanticTestApp(stdin io.Reader) (*App, *bytes.Buffer, *bytes.Buffer) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	return &App{Stdout: stdout, Stderr: stderr, Stdin: stdin}, stdout, stderr
}

func newSemanticCapability(t *testing.T, relays ...string) link.Link {
	t.Helper()
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x5a}, ed25519.SeedSize))
	capability, err := link.NewSenderAuthenticated(
		bytes.Repeat([]byte{0xa5}, link.ReadSecretBytes),
		privateKey.Public().(ed25519.PublicKey),
		relays,
	)
	if err != nil {
		t.Fatal(err)
	}
	return capability
}

type semanticAdmissionSuspension struct {
	mu        sync.Mutex
	resumeErr error
	resumes   int
}

func (relay *semanticAdmissionSuspension) Resume() error {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	relay.resumes++
	return relay.resumeErr
}

func (relay *semanticAdmissionSuspension) resumeCount() int {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	return relay.resumes
}

type semanticAdmissionClock struct {
	mu     sync.Mutex
	now    time.Time
	timer  receiverAdmissionTimer
	delays []time.Duration
}

func (clock *semanticAdmissionClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *semanticAdmissionClock) NewTimer(delay time.Duration) receiverAdmissionTimer {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.delays = append(clock.delays, delay)
	return clock.timer
}

func (clock *semanticAdmissionClock) recordedDelays() []time.Duration {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return append([]time.Duration(nil), clock.delays...)
}

type semanticAdmissionTimer struct {
	channel chan time.Time
	mu      sync.Mutex
	stopped bool
}

func newSemanticAdmissionTimer() *semanticAdmissionTimer {
	return &semanticAdmissionTimer{channel: make(chan time.Time)}
}

func (timer *semanticAdmissionTimer) C() <-chan time.Time { return timer.channel }

func (timer *semanticAdmissionTimer) Stop() bool {
	timer.mu.Lock()
	defer timer.mu.Unlock()
	wasActive := !timer.stopped
	timer.stopped = true
	return wasActive
}
