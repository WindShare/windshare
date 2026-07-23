package cli

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

const (
	getJoinWindow       = 10 * time.Second
	getProgressInterval = 500 * time.Millisecond
)

type getRequest struct {
	outDir string
	only   []string
	link   link.Link
}

func (a *App) runGet(ctx context.Context, args []string) int {
	request, code := a.parseGetRequest(args)
	if code != ExitOK {
		return code
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
		func() *activeReceiverPeer { return a.startReceiverPeer(ctx, runtime, observePeer) },
		func() (transfer.SelectionRules, error) { return selectionRules(request.only) },
	)
	if peer != nil {
		defer peer.Close()
	}
	if err != nil {
		a.logf("get: resolve selection: %v", err)
		return ExitUsage
	}
	output, err := prepared.OpenOutput(ctx, request.outDir)
	if err != nil {
		a.logf("get: open durable output session: %v", err)
		return ExitFailure
	}
	if output.Reopened {
		a.logf("get: resumed a durable output session")
	}
	if output.Quarantined > 0 {
		a.logf("get: quarantined %d unrelated or corrupt output journal(s)", output.Quarantined)
	}
	job, err := runtime.NewTransferJob(rules, output.Session)
	if err != nil {
		_ = output.Session.AbortJob(context.Background(), err)
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

func (a *App) parseGetRequest(args []string) (getRequest, int) {
	flags := a.newFlagSet("get")
	outDir := flags.String("o", ".", "output directory")
	keyString := flags.String("key", "", "separate key string when the link has no fragment")
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
	return getRequest{outDir: *outDir, only: append([]string(nil), only...), link: capability}, ExitOK
}

func (a *App) resolveLink(raw, keyString string) (link.Link, error) {
	if keyString != "" {
		return link.Merge(raw, keyString)
	}
	capability, err := link.Parse(raw)
	if !errors.Is(err, link.ErrMissingFragment) {
		return capability, err
	}
	_, _ = fmt.Fprint(a.stderrWriter(), "Link has no key; enter the key string: ")
	line, readErr := bufio.NewReader(a.Stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		if readErr != nil {
			return link.Link{}, fmt.Errorf("read key string: %w", readErr)
		}
		return link.Link{}, errors.New("no key string was provided")
	}
	return link.Merge(raw, line)
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
		a.logf("get: directory %q failed at stage %d: %v", failure.Path, failure.Stage, failure.Err)
	}
	for _, failure := range result.Files {
		a.logf("get: file %q failed at stage %d: %v", failure.Path, failure.Stage, failure.Err)
	}
	switch result.Outcome {
	case transfer.JobSucceeded:
		a.logf("get: completed %d file(s), %d byte(s)", result.SucceededFiles, result.Measure.DiscoveredBytes)
		return ExitOK
	case transfer.JobCompletedWithErrors:
		if transferResultDrifted(result) {
			return ExitDrift
		}
		return ExitFailure
	case transfer.JobAborted:
		if errors.Is(result.AbortCause, transfer.ErrSelectionTargetMissing) {
			a.logf("get: selection target was not found: %v", result.AbortCause)
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
		err := errors.Join(result.AbortCause, runtimeErr, connectionErr)
		if classifyTransferAbort(result.AbortCause, runtimeErr, connectionErr) == ExitNetwork {
			a.logf("get: transfer aborted: %v", err)
			return ExitNetwork
		}
		if ctx.Err() != nil {
			a.logf("get: interrupted")
			return ExitFailure
		}
		a.logf("get: transfer aborted: %v", err)
		return ExitFailure
	default:
		a.logf("get: transfer returned an invalid outcome")
		return ExitFailure
	}
}

func classifyTransferAbort(cause, runtimeErr, connectionErr error) int {
	if runtimeErr != nil || connectionErr != nil || transfer.IsSessionFailure(cause) {
		return ExitNetwork
	}
	return ExitFailure
}

func transferResultDrifted(result transfer.JobResult) bool {
	if errors.Is(result.AbortCause, content.ErrRevisionStale) || errors.Is(result.AbortCause, content.ErrSourceDrift) ||
		errors.Is(result.AbortCause, catalog.ErrDirectoryStale) {
		return true
	}
	for _, failure := range result.Directories {
		if errors.Is(failure.Err, catalog.ErrDirectoryStale) {
			return true
		}
	}
	for _, failure := range result.Files {
		if errors.Is(failure.Err, content.ErrRevisionStale) || errors.Is(failure.Err, content.ErrSourceDrift) {
			return true
		}
	}
	return false
}
