package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/internal/testrun"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

const (
	shareStopTimeout      = 20 * time.Second
	shareServeJoinTimeout = time.Second
)

type shareRequest struct {
	paths       []string
	relayURL    string
	frontURL    string
	chunkSize   uint32
	splitKey    bool
	observation observationOptions
}

type shareSessionFactory interface {
	AdmitChannel(context.Context, protocolsession.FrameChannel) (sessionruntime.SenderChannelAdmission, error)
	Stop(context.Context, string) error
}

type activeShare struct {
	lifecycle    *senderRelayLifecycle
	factory      shareSessionFactory
	prepared     *liveshare.PreparedSender
	runtime      *commandRuntime
	observations *shareObservations
	startedAt    time.Time
}

func (a *App) runShare(ctx context.Context, args []string) int {
	request, parse := a.parseShareRequest(args)
	if parse != requestParseReady {
		return parse.exitCode()
	}
	runtime, err := a.newCommandRuntime(clievent.CommandShare, request.observation)
	if err != nil {
		if !errors.Is(err, errUserTraceOpen) {
			_, _ = fmt.Fprintln(a.stderrWriter(), "share: command observation could not start")
		}
		return ExitFailure
	}
	startedAt := runtime.Clock().Now()
	observations := newShareObservations(runtime)
	defer func() {
		observations.completeWithin()
		runtime.FinalizeStaged()
		runtime.Close()
	}()

	prepared, code := a.prepareShareSender(ctx, request, runtime.Clock(), runtime, observations)
	if code != ExitOK {
		return code
	}
	defer func() { _ = prepared.Close() }()
	lifecycle, relayAuthority, code := a.connectShareRelay(
		ctx,
		prepared,
		request.relayURL,
		runtime,
		observations,
	)
	if code != ExitOK {
		return code
	}
	active, code := a.activateShare(
		prepared,
		lifecycle,
		relayAuthority,
		request,
		runtime,
		observations,
		startedAt,
	)
	if code != ExitOK {
		return code
	}
	return a.serveActiveShare(ctx, active)
}

func (a *App) prepareShareSender(
	ctx context.Context,
	request shareRequest,
	clock commandClock,
	emitter shareCommandPublisher,
	observations *shareObservations,
) (*liveshare.PreparedSender, int) {
	prepared, err := liveshare.PrepareSender(ctx, liveshare.SenderConfig{
		Paths: request.paths, Relays: []string{request.relayURL}, ChunkSize: request.chunkSize,
		Random: rand.Reader, Now: clock.Now,
		CatalogTracer: observations, RootPrefetchTracer: observations,
	})
	if err != nil {
		emitShareCommandFailure(emitter, ExitUsage, err)
		return nil, ExitUsage
	}
	if err := prepared.AuthorizeRegistration(); err != nil {
		_ = prepared.Close()
		emitShareCommandFailure(emitter, ExitFailure, err)
		return nil, ExitFailure
	}
	return prepared, ExitOK
}

func (a *App) connectShareRelay(
	ctx context.Context,
	prepared *liveshare.PreparedSender,
	relayURL string,
	runtime *commandRuntime,
	observations *shareObservations,
) (*senderRelayLifecycle, clievent.RelayAuthority, int) {
	material := prepared.Registration()
	shareID, shareInstance, pkHash, err := relayRegistrationIdentity(material)
	if err != nil {
		emitShareCommandFailure(runtime, ExitFailure, err)
		return nil, clievent.RelayAuthority{}, ExitFailure
	}
	var resumeToken v2.ResumeToken
	if _, err := rand.Read(resumeToken[:]); err != nil {
		emitShareCommandFailure(runtime, ExitFailure, err)
		return nil, clievent.RelayAuthority{}, ExitFailure
	}
	register, err := relayv2.NewFreshRegisterInit(
		shareID,
		shareInstance,
		pkHash,
		material.Descriptor,
		resumeToken,
	)
	if err != nil {
		emitShareCommandFailure(runtime, ExitFailure, err)
		return nil, clievent.RelayAuthority{}, ExitFailure
	}
	connection, err := relayv2.DialSender(ctx, relayv2.SenderConfig{
		RelayBaseURL: relayURL, Init: register, SenderPrivateKey: material.SenderPrivateKey,
		Descriptor: material.Descriptor,
		Dial:       relayv2.DialOptions{LifecycleObservationCapacity: observations.relayObservationCapacity()},
	})
	if err != nil {
		exit := ExitNetwork
		if ctx.Err() != nil {
			exit = ExitFailure
		}
		emitShareCommandFailure(runtime, exit, err)
		return nil, clievent.RelayAuthority{}, exit
	}
	observations.attachRelayStream(connection.LifecycleTrace())
	relayAuthority, err := commandprojection.RelayAuthority(connection.Endpoint())
	if err != nil {
		_ = connection.Close()
		observations.registerRelayCompletion(connection.CompleteObservations)
		emitShareCommandFailure(runtime, ExitFailure, err)
		return nil, clievent.RelayAuthority{}, ExitFailure
	}
	observations.SetRelayAuthority(relayAuthority)
	lifecycle, err := newSenderRelayLifecycle(senderRelayLifecycleConfig{
		relayURL: relayURL, fresh: register, resumeToken: resumeToken,
		privateKey: material.SenderPrivateKey, initial: connection,
		lifecycleObservationCapacity: observations.relayObservationCapacity(),
		observeConnection: func(connection senderRelayConnection) {
			observations.attachRelayStream(connection.LifecycleTrace())
		},
		observe:        a.observeSenderRelayRecovery,
		observeAttempt: observations.ObserveRelayRecovery,
	})
	if err != nil {
		_ = connection.Close()
		observations.registerRelayCompletion(connection.CompleteObservations)
		emitShareCommandFailure(runtime, ExitFailure, err)
		return nil, clievent.RelayAuthority{}, ExitFailure
	}
	observations.registerRelayCompletion(lifecycle.CompleteObservations)
	return lifecycle, relayAuthority, ExitOK
}

func (a *App) activateShare(
	prepared *liveshare.PreparedSender,
	lifecycle *senderRelayLifecycle,
	relayAuthority clievent.RelayAuthority,
	request shareRequest,
	runtime *commandRuntime,
	observations *shareObservations,
	startedAt time.Time,
) (*activeShare, int) {
	factory, err := a.newShareRuntimeFactory(prepared, lifecycle, observations, runtime.Clock())
	if err != nil {
		_ = lifecycle.Cleanup(context.Background())
		emitShareCommandFailure(runtime, ExitFailure, err)
		return nil, ExitFailure
	}
	sharingSubject, err := commandprojection.ProjectSharingSubject(prepared.SelectedRootSummary())
	if err != nil {
		stopShareFactory(factory, "Sender sharing subject projection failed")
		emitShareCommandFailure(runtime, ExitFailure, err)
		return nil, ExitFailure
	}
	relayConnected, err := clievent.NewRelayConnected(clievent.CommandShare, relayAuthority)
	if err != nil {
		stopShareFactory(factory, "Sender relay projection failed")
		emitShareCommandFailure(runtime, ExitFailure, err)
		return nil, ExitFailure
	}
	err = executeSharePublication(sharePublicationPlan{
		buildPayload: func() ([]byte, error) {
			return buildShareCapabilityPayload(prepared.Capability(), request.frontURL, request.splitKey)
		},
		publishPayload: func(payload []byte) error {
			return publishShareCapability(a.Stdout, payload)
		},
		stopRuntime: func() {
			stopShareFactory(factory, "Sender capability publication failed")
		},
		startRootPrefetch: prepared.StartRootPrefetch,
		publishPrivateReady: func() error {
			// Private process readiness remains a correctness milestone and follows
			// the user-visible capability write, but is never copied to user trace.
			a.recordProcessTrace(
				processTraceShareComponent,
				processTraceSenderReady,
				testrun.OutcomeSucceeded,
			)
			return a.processTrace.err()
		},
		publishPublicReady: func() {
			emitShareReady(runtime, sharingSubject, relayConnected)
		},
	})
	if err != nil {
		var publication *sharePublicationFailure
		if !errors.As(err, &publication) {
			emitShareKnownFailure(runtime, ExitFailure, clievent.FailureUnexpected)
			return nil, ExitFailure
		}
		switch publication.stage {
		case sharePublicationBuildFailed:
			emitShareKnownFailure(runtime, ExitUsage, clievent.FailureCapabilityInvalid)
			return nil, ExitUsage
		case sharePublicationWriteFailed:
			emitShareKnownFailure(runtime, ExitFailure, clievent.FailurePublication)
			return nil, ExitFailure
		default:
			emitShareCommandFailure(runtime, ExitFailure, publication.cause)
			return nil, ExitFailure
		}
	}
	return &activeShare{
		lifecycle: lifecycle, factory: factory, prepared: prepared, runtime: runtime,
		observations: observations, startedAt: startedAt,
	}, ExitOK
}

func stopShareFactory(factory shareSessionFactory, message string) {
	stopContext, cancel := context.WithTimeout(context.Background(), shareStopTimeout)
	_ = factory.Stop(stopContext, message)
	cancel()
}

func (a *App) serveActiveShare(ctx context.Context, active *activeShare) int {
	serveDone := make(chan error, 1)
	go func() { serveDone <- a.serveSessions(ctx, active.factory, active.lifecycle, active.observations) }()
	trigger := shareShutdownServeEnded
	var serveErr error
	select {
	case <-ctx.Done():
		trigger = shareShutdownCallerInterrupted
	case serveErr = <-serveDone:
		trigger = shareTriggerAfterServe(ctx.Err(), serveErr)
	}
	stopContext, cancelStop := context.WithTimeout(context.Background(), shareStopTimeout)
	a.recordProcessTrace(processTraceShareComponent, processTraceSenderStop, testrun.OutcomeStarted)
	stopErr := active.factory.Stop(stopContext, "Sender stopped")
	stopOutcome := testrun.OutcomeSucceeded
	if stopErr != nil {
		stopOutcome = testrun.OutcomeFailed
	}
	a.recordProcessTrace(processTraceShareComponent, processTraceSenderStop, stopOutcome)
	cancelStop()
	if trigger == shareShutdownCallerInterrupted && serveErr == nil {
		serveErr = awaitInterruptedShareServe(serveDone, shareServeJoinTimeout)
	}
	// The stopped result is trustworthy only after local catalog/content
	// authority has joined too. Close is idempotent, so runShare's defer remains
	// a fallback for every earlier return without racing this settlement cut.
	runtimeStopErr := stopErr
	releaseErr := active.prepared.Close()
	stopErr = errors.Join(stopErr, releaseErr)
	settlement := settleShareLifecycle(trigger, ctx.Err(), serveErr, stopErr)
	elapsed := max(active.runtime.Clock().Now().Sub(active.startedAt), 0)
	resultInput := commandprojection.ShareResultInput{
		Clean:   settlement.Err() == nil,
		Failure: settlement.Err(),
		Elapsed: elapsed,
	}
	if !resultInput.Clean {
		resultInput.FailureClass = commandprojection.ShareFailureNetwork
		if settlement.serve.failure == nil && runtimeStopErr == nil && releaseErr != nil {
			resultInput.FailureClass = commandprojection.ShareFailureLocal
		}
	}
	result, err := commandprojection.ProjectShareResult(resultInput)
	if err != nil {
		emitShareKnownFailure(active.runtime, ExitFailure, clievent.FailureUnexpected)
		return ExitFailure
	}
	event, err := clievent.NewSharingStopped(result)
	if err != nil {
		emitShareKnownFailure(active.runtime, ExitFailure, clievent.FailureUnexpected)
		return ExitFailure
	}
	active.observations.completeWithin()
	active.runtime.Finalize(event)
	code, ok := result.ExitCode().ProcessCode()
	if !ok {
		return ExitFailure
	}
	return code
}

func (a *App) observeSenderRelayRecovery(milestone senderRelayRecoveryMilestone) {
	outcome := testrun.OutcomeFailed
	switch milestone {
	case senderRelayRecoveryStarted:
		outcome = testrun.OutcomeStarted
	case senderRelayRecoverySucceeded:
		outcome = testrun.OutcomeSucceeded
	case senderRelayRecoveryFailed:
	default:
		return
	}
	a.recordProcessTrace(processTraceShareComponent, processTraceSenderRelayRecovery, outcome)
}

func (a *App) newShareRuntimeFactory(
	prepared *liveshare.PreparedSender,
	lifecycle *senderRelayLifecycle,
	observations *shareObservations,
	clock commandClock,
) (*sessionruntime.SenderFactory, error) {
	peers, err := a.newSenderPeerFactory(observations, clock)
	if err != nil {
		return nil, fmt.Errorf("initialize direct peer connectivity: %w", err)
	}
	return prepared.NewRuntimeFactory(liveshare.RuntimeFactoryConfig{
		TerminalConnectivity:    lifecycle,
		PeerHandlers:            peers,
		TerminalSendObserver:    observations.terminalSendObserver(),
		SessionTerminalObserver: observations.sessionTerminalObserver(),
		ProtocolTracer:          observations.protocolTracer(),
	})
}

func (a *App) parseShareRequest(args []string) (shareRequest, requestParseOutcome) {
	flags := a.newFlagSet("share")
	relayURL := flags.String("relay", DefaultRelayURL, "relay server base URL")
	blockSize := flags.Int64("block-size", 0, "file-local block size in bytes; 0 uses 1 MiB")
	splitKey := flags.Bool("split-key", false, "print a bare link and separate key string")
	frontURL := flags.String("front-url", DefaultFrontURL, "frontend base URL embedded in the link")
	var observation observationOptions
	if err := bindObservationOptions(flags, &observation); err != nil {
		_, _ = fmt.Fprintln(a.stderrWriter(), "share: observation options are unavailable")
		return shareRequest{}, requestParseInternalFailure
	}
	paths, flagParse := parseInterleaved(flags, args)
	if parse := a.projectFlagParse("share", flags, "share <path...> [options]", flagParse); parse != requestParseReady {
		return shareRequest{}, parse
	}
	if err := observation.validate(); err != nil {
		_, _ = fmt.Fprintf(a.stderrWriter(), "share: %s\n", observationOptionDiagnostic(err))
		return shareRequest{}, requestParseUsageFailure
	}
	if len(paths) == 0 || *relayURL == "" || *frontURL == "" {
		_, _ = fmt.Fprintln(a.stderrWriter(), "share: at least one path, a relay URL, and a frontend URL are required")
		return shareRequest{}, requestParseUsageFailure
	}
	chunkSize := int64(catalog.DefaultChunkSize)
	if *blockSize != 0 {
		chunkSize = *blockSize
	}
	if chunkSize < 0 || chunkSize > math.MaxUint32 {
		_, _ = fmt.Fprintln(a.stderrWriter(), "share: block size is outside the suite-02 range")
		return shareRequest{}, requestParseUsageFailure
	}
	return shareRequest{
		paths: paths, relayURL: *relayURL, frontURL: *frontURL,
		chunkSize: uint32(chunkSize), splitKey: *splitKey, observation: observation,
	}, requestParseReady
}

func relayRegistrationIdentity(material liveshare.RegistrationMaterial) (v2.ShareID, v2.ShareInstance, v2.PKHash, error) {
	shareID, err := v2.ShareIDFromBytes(material.ShareID)
	if err != nil {
		return v2.ShareID{}, v2.ShareInstance{}, v2.PKHash{}, err
	}
	shareInstance, err := v2.ShareInstanceFromBytes(material.ShareInstance)
	if err != nil {
		return v2.ShareID{}, v2.ShareInstance{}, v2.PKHash{}, err
	}
	pkHash, err := v2.PKHashFromBytes(material.PKHash)
	return shareID, shareInstance, pkHash, err
}

func (a *App) serveSessions(
	ctx context.Context,
	factory shareSessionFactory,
	lifecycle *senderRelayLifecycle,
	observations *shareObservations,
) error {
	for {
		channel, err := lifecycle.Accept(ctx)
		if err != nil {
			return err
		}
		go func() {
			admission, err := factory.AdmitChannel(ctx, channel)
			if err != nil {
				_ = channel.Close()
				return
			}
			if admission.Kind == sessionruntime.SenderChannelAttachedLane {
				return
			}
			session := admission.Session
			if session == nil {
				_ = channel.Close()
				observations.projectionFailed(clievent.ObserverLossCommandAdapter, commandprojection.ErrInvalidProjection)
				return
			}
			<-session.Done()
			session.Close()
			// The private milestone is the correctness cut after transport and factory
			// authority have both retired; user-facing stderr is not a synchronization API.
			a.recordProcessTrace(
				processTraceShareComponent,
				processTraceSenderSessionRetired,
				testrun.OutcomeSucceeded,
			)
		}()
	}
}

func emitShareCommandFailure(emitter shareCommandPublisher, exit int, cause error) {
	if emitter == nil {
		return
	}
	event, err := commandprojection.ProjectCommandFailure(
		clievent.CommandShare,
		clievent.ExitCode(exit),
		cause,
	)
	if err != nil {
		emitShareKnownFailure(emitter, ExitFailure, clievent.FailureUnexpected)
		return
	}
	if finalizer, ok := emitter.(shareCommandFinalizer); ok {
		finalizer.StageFinalization(event)
		return
	}
	emitter.Publish(event)
}

func emitShareKnownFailure(emitter shareCommandPublisher, exit int, code clievent.FailureCode) {
	if emitter == nil {
		return
	}
	event, err := clievent.NewCommandFailed(
		clievent.CommandShare,
		clievent.ExitCode(exit),
		mustShareFailure(code),
	)
	if err == nil {
		if finalizer, ok := emitter.(shareCommandFinalizer); ok {
			finalizer.StageFinalization(event)
			return
		}
		emitter.Publish(event)
	}
}

func emitShareReady(
	emitter shareCommandPublisher,
	sharingSubject clievent.SharingSubjectSelected,
	relayConnected clievent.RelayConnected,
) {
	if emitter == nil {
		return
	}
	emitter.Publish(clievent.NewReady(), sharingSubject, relayConnected)
}
