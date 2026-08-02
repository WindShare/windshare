package cli

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/internal/testrun"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

const (
	getJoinWindow                  = 10 * time.Second
	getProgressInterval            = 500 * time.Millisecond
	outputTraceIdentityPrefixBytes = 8
)

type getRequest struct {
	outDir       string
	only         []string
	link         link.Link
	connectivity ConnectivityPolicy
}

func (a *App) runGet(ctx context.Context, args []string) int {
	request, code := a.parseGetRequest(args)
	if code != ExitOK {
		return code
	}
	outputRoot, err := filepath.Abs(request.outDir)
	if err != nil {
		a.logf("get: resolve output directory: %v", err)
		return ExitFailure
	}
	output, err := osfs.NewFilesystemOutputAuthority(osfs.FilesystemOutputAuthorityConfig{
		RootPath: outputRoot, CreateRoot: true,
		Tracer: osfs.FilesystemOutputTraceFunc(a.traceFilesystemOutput),
	})
	if err != nil {
		a.logf("get: initialize output authority: %v", err)
		return ExitFailure
	}
	connection, code := a.dialV2Receiver(ctx, request.link)
	if code != ExitOK {
		return code
	}
	defer connection.Close()
	prepared, err := liveshare.PrepareReceiver(liveshare.ReceiverConfig{
		Capability: request.link, DescriptorObject: connection.Descriptor(),
		PeerControls: v2signal.ReceiverControlValidator{},
	})
	if err != nil {
		a.logf("get: authenticate descriptor: %v\nCheck that the link and key belong to this share.", err)
		return ExitUsage
	}
	defer prepared.Close()
	runtime, err := prepared.Connect(ctx, connection.Channel())
	if err != nil {
		if ctx.Err() != nil {
			a.logf("get: interrupted during session handshake")
			return ExitFailure
		}
		a.logf("get: establish authenticated session: %v", err)
		return ExitNetwork
	}
	defer runtime.Close()
	clock := a.admissionClock()
	downloadT0 := clock.Now()
	initialLaneID, initialLaneEpoch := runtime.LaneIdentity()
	relaySuspension, err := runtime.LaneSet().SuspendContent(
		transfer.LaneIdentity{ID: initialLaneID, Epoch: initialLaneEpoch},
	)
	if err != nil {
		a.logf("get: initialize content-path admission: %v", err)
		return ExitFailure
	}
	admission, err := newRelayContentAdmission(
		downloadT0,
		clock,
		relaySuspension,
	)
	if err != nil {
		a.logf("get: initialize content-path admission: %v", err)
		return ExitFailure
	}
	admissionMonitorDone := a.monitorReceiverAdmission(admission, runtime)
	defer func() {
		admission.Close()
		admission.Wait()
		<-admissionMonitorDone
		a.logReceiverAdmissionTraces(runtime.ProtocolSessionID().Bytes(), admission)
	}()
	observePeer := func(signal receiverPeerSignal) {
		if observeErr := admission.ObservePeer(signal); observeErr != nil {
			a.logf("get: apply direct-peer admission signal failed cause_class=relay_resume")
			runtime.Close()
		}
	}
	peer, rules, err := beginReceiverPlanning(
		request.connectivity,
		func() *activeReceiverPeer { return a.startReceiverPeer(ctx, runtime, observePeer) },
		func() { observePeer(receiverPeerFailed) },
		func() (transfer.SelectionRules, error) { return selectionRules(request.only) },
	)
	if peer != nil {
		defer peer.Close()
	}
	if err != nil {
		a.logf("get: resolve selection: %v", err)
		return ExitUsage
	}
	job, err := runtime.NewTransferJob(rules, output)
	if err != nil {
		a.logf("get: initialize transfer: %v", err)
		return ExitFailure
	}
	result := a.runTransferJob(ctx, job, func(measure transfer.SelectionMeasure) {
		if observeErr := admission.ObserveSelection(measure.Class()); observeErr != nil {
			a.logf("get: apply selection admission signal: %v", observeErr)
			runtime.Close()
		}
	})
	admission.Close()
	admission.Wait()
	<-admissionMonitorDone
	return a.reportTransferResultWithAdmission(ctx, runtime, connection, result, admission.Err())
}

// Native output emits dense per-file lifecycle traces. The CLI keeps recovery,
// attention, and failure boundaries while suppressing routine publish noise so
// diagnostics stay reconstructable without turning a large transfer into a log.
func (a *App) traceFilesystemOutput(event osfs.FilesystemOutputTrace) {
	if !shouldLogFilesystemOutputTrace(event) {
		return
	}
	intent := outputTraceIdentity(event.ResumeIntent.Bytes())
	session := outputTraceIdentity(event.SessionID.Bytes())
	locator := outputTraceIdentity(event.LocatorDigest[:])
	a.logf(
		"get: output trace operation=%d resume_intent=%s session=%s locator=%s certification=%q recovery_action=%q file_settlement=%s job_settlement=%s quarantine_reason=%d ancestry_boundary=%d ancestry_decision=%d ancestry_claims=%d native_lock_scope=%d native_lock_milestone=%d state_install_stage=%d state_generation=%d failure_scope=%d failure_code=%d mutation_failure=%t parent_sync_failure=%t failed=%t",
		event.Operation, intent, session, locator, event.Certification, event.RecoveryAction.String(),
		fileSettlementName(event.FileSettlement), jobSettlementName(event.JobSettlement), event.QuarantineReason,
		event.AncestryBoundary, event.AncestryDecision, event.AncestryClaimCount,
		event.NativeLockScope, event.NativeLockMilestone, event.StateInstallStage, event.StateGeneration,
		event.FailureScope, event.FailureCode, event.MutationReportedFailure,
		event.ParentSyncReportedFailure, event.Failed,
	)
}

func shouldLogFilesystemOutputTrace(event osfs.FilesystemOutputTrace) bool {
	if event.Failed {
		return true
	}
	switch event.Operation {
	case osfs.TraceFilesystemCertified, osfs.TraceSessionOpened,
		osfs.TraceSessionSettlement, osfs.TraceStateInstallCutAdopted:
		return true
	case osfs.TraceFileRecoveryDecision:
		return event.RecoveryAction == osfs.FilesystemOutputRecoveryResumeContent ||
			event.RecoveryAction == osfs.FilesystemOutputRecoveryInstallQuarantine ||
			event.RecoveryAction == osfs.FilesystemOutputRecoveryHoldQuarantine
	case osfs.TraceFileSettlement:
		return event.FileSettlement != transfer.FilePublished
	case osfs.TraceNativeLock:
		return event.NativeLockMilestone == osfs.FilesystemOutputNativeLockContended
	default:
		return false
	}
}

func outputTraceIdentity(raw []byte) string {
	nonzero := false
	for _, value := range raw {
		nonzero = nonzero || value != 0
	}
	if !nonzero {
		return "-"
	}
	if len(raw) > outputTraceIdentityPrefixBytes {
		raw = raw[:outputTraceIdentityPrefixBytes]
	}
	return hex.EncodeToString(raw)
}

type capabilityInputErrorKind uint8

const (
	capabilityInputInvalid capabilityInputErrorKind = iota + 1
	capabilityInputKeyMissing
)

const (
	invalidCapabilityDiagnostic    = "invalid capability link"
	missingCapabilityKeyDiagnostic = "key string is required"
)

type capabilityInputError struct {
	kind  capabilityInputErrorKind
	cause error
}

func (failure *capabilityInputError) Error() string {
	if failure.kind == capabilityInputKeyMissing {
		return missingCapabilityKeyDiagnostic
	}
	return invalidCapabilityDiagnostic
}

func (failure *capabilityInputError) Unwrap() error { return failure.cause }

func invalidCapabilityInput(cause error) error {
	return &capabilityInputError{kind: capabilityInputInvalid, cause: cause}
}

func missingCapabilityKey(cause error) error {
	return &capabilityInputError{kind: capabilityInputKeyMissing, cause: cause}
}

func (a *App) parseGetRequest(args []string) (getRequest, int) {
	flags := a.newFlagSet("get")
	outDir := flags.String("o", ".", "output directory")
	keyString := flags.String("key", "", "separate key string when the link has no fragment")
	connectivityName := flags.String(
		"connectivity",
		ConnectivityAuto.String(),
		"content connectivity policy: auto or relay-only",
	)
	var only repeatedFlag
	flags.Var(&only, "only", "download only this catalog path; repeatable, directories include descendants")
	positional, err := parseInterleaved(flags, args)
	if err != nil {
		return getRequest{}, ExitUsage
	}
	if len(positional) != 1 {
		a.logf("get: exactly one link argument is required")
		return getRequest{}, ExitUsage
	}
	connectivity, err := ParseConnectivityPolicy(*connectivityName)
	if err != nil {
		a.logf("get: %v", err)
		return getRequest{}, ExitUsage
	}
	capability, err := a.resolveLink(positional[0], *keyString)
	if err != nil {
		a.logf("get: %v", err)
		return getRequest{}, ExitUsage
	}
	if capability.Suite != link.SuiteSenderAuthenticated {
		a.logf("get: this build accepts only suite-02 links")
		return getRequest{}, ExitUsage
	}
	if len(capability.Relays) == 0 {
		a.logf("get: link has no relay address (?r=)")
		return getRequest{}, ExitUsage
	}
	return getRequest{
		outDir: *outDir, only: append([]string(nil), only...), link: capability, connectivity: connectivity,
	}, ExitOK
}

func (a *App) resolveLink(raw, keyString string) (link.Link, error) {
	if keyString != "" {
		capability, err := link.Merge(raw, keyString)
		if err != nil {
			return link.Link{}, invalidCapabilityInput(err)
		}
		return capability, nil
	}
	capability, err := link.Parse(raw)
	if !errors.Is(err, link.ErrMissingFragment) {
		if err != nil {
			return link.Link{}, invalidCapabilityInput(err)
		}
		return capability, nil
	}
	_, _ = fmt.Fprint(a.stderrWriter(), "Link has no key; enter the key string: ")
	line, readErr := bufio.NewReader(a.Stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		if readErr != nil {
			return link.Link{}, missingCapabilityKey(fmt.Errorf("read key string: %w", readErr))
		}
		return link.Link{}, missingCapabilityKey(errors.New("no key string was provided"))
	}
	capability, err = link.Merge(raw, line)
	if err != nil {
		return link.Link{}, invalidCapabilityInput(err)
	}
	return capability, nil
}

func (a *App) dialV2Receiver(ctx context.Context, capability link.Link) (*relayv2.ReceiverConnection, int) {
	rawShareID, err := base64.RawURLEncoding.Strict().DecodeString(capability.ShareID)
	if err != nil {
		a.logf("get: invalid suite-02 share identity")
		return nil, ExitUsage
	}
	shareID, err := v2.ShareIDFromBytes(rawShareID)
	if err != nil {
		a.logf("get: invalid suite-02 share identity")
		return nil, ExitUsage
	}
	joinContext, cancel := context.WithTimeout(ctx, getJoinWindow)
	defer cancel()
	for {
		connection, err := relayv2.DialReceiver(joinContext, relayv2.ReceiverConfig{
			RelayBaseURL: capability.Relays[0], ShareID: shareID,
		})
		if err == nil {
			return connection, ExitOK
		}
		var relayError *relayv2.RelayError
		if !errors.As(err, &relayError) || relayError.Code != v2.ErrorStarting {
			if errors.As(err, &relayError) && relayError.Code == v2.ErrorStopped {
				a.recordProcessTrace(
					processTraceGetComponent,
					processTraceReceiverJoinStopped,
					testrun.OutcomeFailed,
				)
			}
			if ctx.Err() != nil {
				a.logf("get: interrupted")
				return nil, ExitFailure
			}
			a.logf("get: connect to relay: %v", err)
			return nil, ExitNetwork
		}
		delay := relayError.RetryAfter
		if delay <= 0 {
			delay = 250 * time.Millisecond
		}
		select {
		case <-joinContext.Done():
			a.logf("get: share did not become ready: %v", joinContext.Err())
			return nil, ExitNetwork
		case <-time.After(delay):
		}
	}
}

func selectionRules(requested []string) (transfer.SelectionRules, error) {
	if len(requested) == 0 {
		return transfer.NewSelectionRules(true, nil)
	}
	return transfer.NewPathSelectionRules(requested)
}

func (a *App) runTransferJob(
	ctx context.Context,
	job *transfer.TransferJob,
	observeSelection func(transfer.SelectionMeasure),
) transfer.JobResult {
	measures := job.SelectionMeasures()
	result := make(chan transfer.JobResult, 1)
	go func() { result <- job.Run(ctx) }()
	ticker := time.NewTicker(getProgressInterval)
	defer ticker.Stop()
	for {
		select {
		case completed := <-result:
			for measures != nil {
				measure, ok := <-measures
				if !ok {
					measures = nil
					continue
				}
				if observeSelection != nil {
					observeSelection(measure)
				}
			}
			return completed
		case measure, ok := <-measures:
			if !ok {
				measures = nil
				continue
			}
			if observeSelection != nil {
				observeSelection(measure)
			}
		case <-ticker.C:
			measure := job.Measure()
			a.logf("get: discovered %d file(s), %d byte(s)", measure.DiscoveredFiles, measure.DiscoveredBytes)
		}
	}
}

func (a *App) reportTransferResultWithAdmission(
	ctx context.Context,
	runtime *sessionruntime.ReceiverRuntime,
	connection *relayv2.ReceiverConnection,
	result transfer.JobResult,
	admissionErr error,
) int {
	for _, failure := range result.Directories {
		a.logf("get: directory %q failed at stage %d: %v", failure.Path, failure.Stage, failure.Cause)
	}
	for _, failure := range result.Files {
		a.logf(
			"get: file %q failed at stage %d settlement=%s: %v",
			failure.Path, failure.Stage, fileSettlementName(failure.Settlement.Kind()), failure.Cause,
		)
		if failure.SettlementFailure != nil {
			a.logf("get: file %q output settlement failed: %v", failure.Path, failure.SettlementFailure)
		}
		if failure.LeaseReleaseFailure != nil {
			a.logf("get: file %q revision lease release failed: %v", failure.Path, failure.LeaseReleaseFailure)
		}
	}
	if result.SettlementFailure != nil {
		a.logf("get: durable output settlement failed: %v", result.SettlementFailure)
	}
	switch result.Settlement.Kind() {
	case transfer.JobPaused:
		a.logf("get: transfer paused; verified progress was retained")
	case transfer.JobPausedNeedsAttention:
		a.logf("get: durable output state was retained and needs attention")
	}
	switch result.Outcome {
	case transfer.JobSucceeded:
		if result.TerminationCause != nil || result.SettlementFailure != nil {
			a.logf("get: transfer returned success with terminal failure state")
			return ExitFailure
		}
		a.logf("get: completed %d file(s), %d byte(s)", result.SucceededFiles, result.Measure.DiscoveredBytes)
		if result.Settlement.Kind() == transfer.JobPausedNeedsAttention {
			return ExitFailure
		}
		if result.Settlement.Kind() != transfer.JobClosed {
			a.logf("get: transfer returned success without a closed output settlement")
			return ExitFailure
		}
		return ExitOK
	case transfer.JobCompletedWithErrors:
		a.logf(
			"get: completed %d file(s) with %d file failure(s) and %d directory failure(s)",
			result.SucceededFiles, len(result.Files), len(result.Directories),
		)
		if transferResultDrifted(result) {
			return ExitDrift
		}
		return ExitFailure
	case transfer.JobPausedOutcome:
		if errors.Is(result.TerminationCause, transfer.ErrSelectionTargetMissing) {
			a.logf("get: selection target was not found: %v", result.TerminationCause)
			return ExitUsage
		}
		if transferResultDrifted(result) {
			return ExitDrift
		}
		var runtimeErr, connectionErr error
		if runtime != nil {
			runtimeErr = runtime.Err()
		}
		if connection != nil {
			connectionErr = connection.Err()
		}
		runtimeErr = errors.Join(runtimeErr, admissionErr)
		err := errors.Join(result.TerminationCause, result.SettlementFailure, runtimeErr, connectionErr)
		if classifyTransferTermination(result.TerminationCause, runtimeErr, connectionErr) == ExitNetwork {
			a.logf("get: transfer stopped: %v", err)
			return ExitNetwork
		}
		if ctx.Err() != nil {
			a.logf("get: interrupted")
			return ExitFailure
		}
		a.logf("get: transfer stopped: %v", err)
		return ExitFailure
	default:
		a.logf("get: transfer returned an invalid outcome")
		return ExitFailure
	}
}

func classifyTransferTermination(cause, runtimeErr, connectionErr error) int {
	if runtimeErr != nil || connectionErr != nil || transfer.IsSessionFailure(cause) {
		return ExitNetwork
	}
	return ExitFailure
}

func transferResultDrifted(result transfer.JobResult) bool {
	if isTransferDrift(result.TerminationCause) {
		return true
	}
	for _, failure := range result.Directories {
		if isTransferDrift(failure.Cause) {
			return true
		}
	}
	for _, failure := range result.Files {
		if isTransferDrift(failure.Cause) {
			return true
		}
	}
	return false
}

func isTransferDrift(cause error) bool {
	return errors.Is(cause, catalog.ErrDirectoryStale) ||
		errors.Is(cause, content.ErrRevisionStale) ||
		errors.Is(cause, content.ErrSourceDrift) ||
		errors.Is(cause, content.ErrRevisionDrift) ||
		errors.Is(cause, transfer.ErrBlockInvalidated)
}

func fileSettlementName(kind transfer.FileSettlementKind) string {
	switch kind {
	case transfer.FilePublished:
		return "published"
	case transfer.FilePaused:
		return "paused"
	case transfer.FileRetired:
		return "retired"
	case transfer.FileCollision:
		return "collision"
	case transfer.FilePublishBlocked:
		return "publish-blocked"
	case transfer.FileQuarantined:
		return "quarantined"
	default:
		return "none"
	}
}

func jobSettlementName(kind transfer.JobSettlementKind) string {
	switch kind {
	case transfer.JobClosed:
		return "closed"
	case transfer.JobPaused:
		return "paused"
	case transfer.JobPausedNeedsAttention:
		return "paused-needs-attention"
	default:
		return "none"
	}
}
