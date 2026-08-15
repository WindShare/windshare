package cli

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
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
	output, code := a.prepareGetOutput(ctx, request)
	if code != ExitOK {
		return code
	}
	defer func() {
		if err := output.authority.Close(); err != nil {
			a.logf("get: close output authority: %v", err)
		}
	}()
	session, code := a.connectGetReceiver(ctx, request.link)
	if code != ExitOK {
		return code
	}
	defer session.Close()
	execution, code := a.prepareGetTransfer(ctx, request, output, session.runtime)
	if code != ExitOK {
		return code
	}
	defer execution.Close()

	result := a.runTransferJob(ctx, execution.job, func(measure transfer.SelectionMeasure) {
		if observeErr := execution.admission.ObserveConnectionSize(measure.ConnectionSizeClass()); observeErr != nil {
			a.logf("get: apply selection admission signal: %v", observeErr)
			session.runtime.Close()
		}
	})
	execution.SettleAdmission()
	return a.reportTransferResultWithAdmission(
		ctx, session.runtime, session.connection, result, execution.admission.Err(), execution.renamed,
	)
}

func (a *App) traceFilesystemOutput(event osfs.FilesystemOutputTrace) {
	a.traceGet(GetTraceEvent{Stage: GetTraceFilesystemOutput, FilesystemOutput: event})
}

func (a *App) traceTransferLifecycle(event transfer.TransferLifecycleTrace) {
	a.traceGet(GetTraceEvent{Stage: GetTraceTransferLifecycle, TransferLifecycle: event})
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
		"content connectivity policy: auto, relay-only, or p2p-only",
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
	progress := a.getProgressReporter()
	defer progress.Finish()
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
			progress.Update(job.Measure())
		}
	}
}

func (a *App) reportTransferResultWithAdmission(
	ctx context.Context,
	runtime *sessionruntime.ReceiverRuntime,
	connection *relayv2.ReceiverConnection,
	result transfer.JobResult,
	admissionErr error,
	renamed bool,
) int {
	a.logTransferFailures(result)
	code := ExitFailure
	switch result.Outcome {
	case transfer.DirectTreeOutcomeSuccess:
		code = a.reportSuccessfulTransfer(result)
	case transfer.DirectTreeOutcomePartial:
		code = a.reportTransferWithErrors(result)
	case transfer.DirectTreeOutcomePaused, transfer.DirectTreeOutcomeFailed:
		code = a.reportPausedTransfer(ctx, runtime, connection, result, admissionErr)
	default:
		a.logf("get: transfer returned an invalid outcome")
	}
	a.logGetTransferSummary(result, renamed)
	return code
}

func (a *App) logTransferFailures(result transfer.JobResult) {
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
}

func (a *App) reportSuccessfulTransfer(result transfer.JobResult) int {
	if result.TerminationCause != nil || result.SettlementFailure != nil {
		a.logf("get: transfer returned success with terminal failure state")
		return ExitFailure
	}
	if result.Measure.Discovery != transfer.DiscoveryComplete || !result.Measure.DiscoveryTerminalSuccess {
		a.logf("get: transfer returned success before discovery completed; selection remains unknown/partial")
		return ExitFailure
	}
	if successfulTransferIncomplete(result) {
		a.logf(
			"get: transfer returned success with incomplete output discovered_files=%d completed_files=%d succeeded_files=%d discovered_bytes=%d completed_bytes=%d",
			result.Measure.DiscoveredFiles, result.Measure.CompletedFiles, result.SucceededFiles,
			result.Measure.DiscoveredBytes, result.Measure.CompletedBytes,
		)
		return ExitFailure
	}
	if result.Settlement.Kind() == transfer.DirectTreeSettlementFailed {
		a.logf("get: durable output state needs attention")
		return ExitFailure
	}
	if result.Settlement.Kind() != transfer.DirectTreeSettlementSuccess {
		a.logf("get: transfer returned success without a published tree settlement")
		return ExitFailure
	}
	if !successfulGetResult(result) {
		a.logf("get: transfer returned success with non-success file or directory outcomes")
		return ExitFailure
	}
	return ExitOK
}

func successfulTransferIncomplete(result transfer.JobResult) bool {
	return result.FileOutcomes.PublishedFiles() != result.SucceededFiles ||
		result.SucceededFiles != result.Measure.CompletedFiles ||
		result.Measure.CompletedFiles != result.Measure.DiscoveredFiles ||
		result.Measure.CompletedBytes != result.Measure.DiscoveredBytes
}

func (a *App) reportTransferWithErrors(result transfer.JobResult) int {
	if missing := missingSelectionTargetFailure(result); missing != nil {
		return a.reportMissingSelectionTarget(result, missing)
	}
	if transferResultDrifted(result) {
		return ExitDrift
	}
	return ExitFailure
}

func (a *App) reportPausedTransfer(
	ctx context.Context,
	runtime *sessionruntime.ReceiverRuntime,
	connection *relayv2.ReceiverConnection,
	result transfer.JobResult,
	admissionErr error,
) int {
	if transferResultDrifted(result) {
		return ExitDrift
	}
	runtimeErr, connectionErr := receiverTerminationErrors(runtime, connection)
	runtimeErr = errors.Join(runtimeErr, admissionErr)
	err := errors.Join(result.TerminationCause, result.SettlementFailure, runtimeErr, connectionErr)
	if classifyTransferTermination(result.TerminationFault, runtimeErr, connectionErr) == ExitNetwork {
		if err != nil {
			a.logf("get: transfer stopped: %v", err)
		} else {
			a.logf("get: authenticated session stopped before the transfer completed")
		}
		return ExitNetwork
	}
	if ctx.Err() != nil {
		a.logf("get: interrupted")
		return ExitFailure
	}
	switch {
	case err != nil:
		a.logf("get: transfer stopped: %v", err)
	case result.Settlement.Kind() == transfer.DirectTreeSettlementFailed:
		a.logf("get: durable output state needs attention")
	case result.Outcome == transfer.DirectTreeOutcomePaused:
		a.logf("get: transfer paused before completion")
	default:
		a.logf("get: transfer failed before completion")
	}
	return ExitFailure
}

func missingSelectionTargetFailure(result transfer.JobResult) error {
	if errors.Is(result.SelectionResolutionFailure, transfer.ErrSelectionTargetMissing) {
		return result.SelectionResolutionFailure
	}
	return nil
}

func (a *App) reportMissingSelectionTarget(result transfer.JobResult, cause error) int {
	if result.Measure.Discovery != transfer.DiscoveryComplete || !result.Measure.DiscoveryTerminalSuccess {
		a.logf("get: selection target remains unknown/partial because discovery did not complete")
		return ExitFailure
	}
	a.logf("get: selection target was not found: %v", cause)
	return ExitUsage
}

func receiverTerminationErrors(
	runtime *sessionruntime.ReceiverRuntime,
	connection *relayv2.ReceiverConnection,
) (runtimeErr error, connectionErr error) {
	if runtime != nil {
		runtimeErr = runtime.Err()
	}
	if connection != nil {
		connectionErr = connection.Err()
	}
	return runtimeErr, connectionErr
}

func classifyTransferTermination(value transferfault.Fault, runtimeErr, connectionErr error) int {
	if runtimeErr != nil || connectionErr != nil ||
		value.Domain() == transferfault.DomainSession && value.Scope() == transferfault.ScopeSessionTerminal {
		return ExitNetwork
	}
	return ExitFailure
}

func transferResultDrifted(result transfer.JobResult) bool {
	return result.SourceDriftFault.Valid()
}

func fileSettlementName(kind transfer.FileSettlementKind) string {
	switch kind {
	case transfer.FilePublished:
		return "published"
	case transfer.FilePaused:
		return "paused"
	case transfer.FileFailed:
		return "retired"
	case transfer.FileCollision:
		return "collision"
	case transfer.FileItemBlocked:
		return "item-blocked"
	default:
		return "none"
	}
}
