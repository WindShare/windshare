package cli

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"github.com/windshare/windshare/cmd/windshare/internal/commandprojection"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/internal/testrun"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

const (
	getJoinWindow              = 10 * time.Second
	getProgressInterval        = 500 * time.Millisecond
	getRelayStartingRetryDelay = 250 * time.Millisecond
)

type getRequest struct {
	outDir       string
	only         []string
	link         link.Link
	connectivity ConnectivityPolicy
	observation  observationOptions
}

func (a *App) runGet(ctx context.Context, args []string) int {
	request, parse := a.parseGetRequest(args)
	if parse != requestParseReady {
		return parse.exitCode()
	}
	runtime, err := a.newCommandRuntime(clievent.CommandGet, request.observation)
	if err != nil {
		return ExitFailure
	}
	defer runtime.Close()
	observation := getObservation{runtime: runtime}
	startedAt := runtime.Clock().Now()

	output, code := a.prepareGetOutput(ctx, request, observation)
	if code != ExitOK {
		return code
	}
	outputClosed := false
	closeOutput := func() {
		if outputClosed {
			return
		}
		outputClosed = true
		if closeErr := output.authority.Close(); closeErr != nil {
			observation.warning(closeErr)
		}
	}
	defer closeOutput()

	session, code := a.connectGetReceiver(ctx, request.link, observation)
	if code != ExitOK {
		return code
	}
	sessionClosed := false
	closeSession := func() {
		if sessionClosed {
			return
		}
		sessionClosed = true
		session.Close()
	}
	defer closeSession()

	execution, code := a.prepareGetTransfer(ctx, request, output, session.runtime, observation)
	if code != ExitOK {
		return code
	}
	defer execution.Close()

	completion := a.runTransferJob(ctx, execution.job, runtime.Clock(), observation, func(progress transfer.ReceiveProgressSnapshot) {
		if observeErr := execution.admission.ObserveConnectionSize(progress.ConnectionSizeClass()); observeErr != nil {
			observation.warning(observeErr)
			session.runtime.Close()
		}
	})
	admissionErr := execution.admission.Err()
	runtimeErr, connectionErr := receiverTerminationErrors(session.runtime, session.connection)
	// Settlement diagnostics must describe the state that ended the transfer.
	// Closing optional paths can discover cleanup residue, but that later residue
	// cannot retroactively turn a caller interruption into a network failure.
	execution.Close()
	// A terminal result is meaningful only after every producer has lost the
	// ability to append a later fact to this run's ordered publication stream.
	closeSession()
	closeOutput()

	return a.reportTransferResultWithAdmission(
		ctx,
		completion,
		admissionErr,
		runtimeErr,
		connectionErr,
		execution.destination,
		execution.destinationAdjusted,
		startedAt,
		observation,
	)
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

func (a *App) parseGetRequest(args []string) (getRequest, requestParseOutcome) {
	flags := a.newFlagSet("get")
	var observation observationOptions
	if err := bindObservationOptions(flags, &observation); err != nil {
		a.writeCompleteLine("get: observation options are unavailable")
		return getRequest{}, requestParseInternalFailure
	}
	outDir := flags.String("o", ".", "output directory")
	keyString := flags.String("key", "", "separate key string when the link has no fragment")
	connectivityName := flags.String(
		"connectivity",
		ConnectivityAuto.String(),
		"content connectivity policy: auto, relay-only, or p2p-only",
	)
	var only repeatedFlag
	flags.Var(&only, "only", "download only this catalog path; repeatable, directories include descendants")
	positional, flagParse := parseInterleaved(flags, args)
	if parse := a.projectFlagParse("get", flags, "get [options] <link>", flagParse); parse != requestParseReady {
		return getRequest{}, parse
	}
	if err := observation.validate(); err != nil {
		a.writeCompleteLine("get: trace output must be a file")
		return getRequest{}, requestParseUsageFailure
	}
	if len(positional) != 1 {
		a.writeCompleteLine("get: exactly one link argument is required")
		return getRequest{}, requestParseUsageFailure
	}
	connectivity, err := ParseConnectivityPolicy(*connectivityName)
	if err != nil {
		a.writeCompleteLine("get: connectivity must be auto, relay-only, or p2p-only")
		return getRequest{}, requestParseUsageFailure
	}
	capability, err := a.resolveLink(positional[0], *keyString)
	if err != nil {
		var failure *capabilityInputError
		if errors.As(err, &failure) && failure.kind == capabilityInputKeyMissing {
			a.writeCompleteLine("get: key string is required")
		} else {
			a.writeCompleteLine("get: invalid capability link")
		}
		return getRequest{}, requestParseUsageFailure
	}
	if capability.Suite != link.SuiteSenderAuthenticated {
		a.writeCompleteLine("get: this build accepts only suite-02 links")
		return getRequest{}, requestParseUsageFailure
	}
	if len(capability.Relays) == 0 {
		a.writeCompleteLine("get: link has no relay address (?r=)")
		return getRequest{}, requestParseUsageFailure
	}
	return getRequest{
		outDir: *outDir, only: append([]string(nil), only...), link: capability, connectivity: connectivity,
		observation: observation,
	}, requestParseReady
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

func (a *App) dialV2Receiver(
	ctx context.Context,
	capability link.Link,
	observation getObservation,
) (*relayv2.ReceiverConnection, int) {
	rawShareID, err := base64.RawURLEncoding.Strict().DecodeString(capability.ShareID)
	if err != nil {
		return nil, observation.commandFailureCode(ExitUsage, clievent.FailureCapabilityInvalid)
	}
	shareID, err := v2.ShareIDFromBytes(rawShareID)
	if err != nil {
		return nil, observation.commandFailureCode(ExitUsage, clievent.FailureCapabilityInvalid)
	}
	joinContext, cancel := context.WithTimeout(ctx, getJoinWindow)
	defer cancel()
	for {
		connection, err := relayv2.DialReceiver(joinContext, relayv2.ReceiverConfig{
			RelayBaseURL: capability.Relays[0], ShareID: shareID,
			Dial: relayv2.DialOptions{
				LifecycleTracer: relayv2.LifecycleTraceFunc(observation.relayLifecycle),
			},
		})
		if err == nil {
			observation.relayConnected(connection.Endpoint())
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
				return nil, observation.commandFailure(ExitFailure, ctx.Err())
			}
			return nil, observation.commandFailure(ExitNetwork, err)
		}
		delay := relayError.RetryAfter
		if delay <= 0 {
			delay = getRelayStartingRetryDelay
		}
		select {
		case <-joinContext.Done():
			return nil, observation.commandFailure(ExitNetwork, joinContext.Err())
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

type getTransferCompletion struct {
	result                  transfer.JobResult
	finalProgress           clievent.TransferProgress
	finalProgressProjection error
}

func (a *App) runTransferJob(
	ctx context.Context,
	job *transfer.TransferJob,
	clock commandClock,
	observation getObservation,
	observeSelection func(transfer.ReceiveProgressSnapshot),
) getTransferCompletion {
	snapshots := job.ProgressSnapshots()
	result := make(chan transfer.JobResult, 1)
	go func() { result <- job.Run(ctx) }()
	receiveOperation := job.ReceiveIntent().OperationID()
	transferJob := job.JobID()
	observation.progress(receiveOperation, transferJob, job.Progress())
	ticker := clock.NewTicker(getProgressInterval)
	defer ticker.Stop()
	for {
		select {
		case completed := <-result:
			for snapshots != nil {
				snapshot, ok := <-snapshots
				if !ok {
					snapshots = nil
					continue
				}
				if observeSelection != nil {
					observeSelection(snapshot)
				}
			}
			progress, projectionErr := commandprojection.ProjectTransferProgress(
				receiveOperation,
				transferJob,
				completed.Progress,
			)
			return getTransferCompletion{
				result:                  completed,
				finalProgress:           progress,
				finalProgressProjection: projectionErr,
			}
		case snapshot, ok := <-snapshots:
			if !ok {
				snapshots = nil
				continue
			}
			if observeSelection != nil {
				observeSelection(snapshot)
			}
		case <-ticker.C():
			observation.progress(receiveOperation, transferJob, job.Progress())
		}
	}
}

func (a *App) reportTransferResultWithAdmission(
	ctx context.Context,
	completion getTransferCompletion,
	admissionErr error,
	runtimeErr error,
	connectionErr error,
	destination string,
	destinationAdjusted bool,
	startedAt time.Time,
	observation getObservation,
) int {
	if completion.finalProgressProjection != nil {
		return observation.commandFailure(ExitFailure, commandprojection.ErrInvalidProjection)
	}
	now := observation.runtime.Clock().Now()
	elapsed := max(now.Sub(startedAt), 0)
	projected, err := commandprojection.ProjectGetResult(commandprojection.GetResultInput{
		Result: completion.result, AdmissionError: admissionErr,
		RuntimeError: runtimeErr, ConnectionError: connectionErr, ContextError: ctx.Err(),
		Elapsed: elapsed, Destination: clievent.NewDisplayPath(destination),
		DestinationAdjusted: destinationAdjusted,
	})
	if err != nil {
		return observation.commandFailure(ExitFailure, commandprojection.ErrInvalidProjection)
	}
	event, err := clievent.NewTransferSettled(projected)
	if err != nil {
		return observation.commandFailure(ExitFailure, commandprojection.ErrInvalidProjection)
	}
	observation.finalize(completion.finalProgress, event)
	code, ok := projected.ExitCode().ProcessCode()
	if !ok {
		return ExitFailure
	}
	return code
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
