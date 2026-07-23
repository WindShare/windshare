package cli

import (
	"context"
	"crypto/rand"
	"fmt"
	"math"
	"time"

	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/sessionruntime"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

const (
	shareStopTimeout      = 20 * time.Second
	shareServeJoinTimeout = time.Second
)

type shareRequest struct {
	paths     []string
	relayURL  string
	frontURL  string
	chunkSize uint32
	splitKey  bool
}

func (a *App) runShare(ctx context.Context, args []string) int {
	request, code := a.parseShareRequest(args)
	if code != ExitOK {
		return code
	}

	prepared, err := liveshare.PrepareSender(ctx, liveshare.SenderConfig{
		Paths: request.paths, Relays: []string{request.relayURL}, ChunkSize: request.chunkSize, Random: rand.Reader,
	})
	if err != nil {
		a.logf("share: prepare selected roots: %v", err)
		return ExitUsage
	}
	defer func() {
		if err := prepared.Close(); err != nil {
			a.logf("share: release local authority: %v", err)
		}
	}()
	if err := prepared.AuthorizeRegistration(); err != nil {
		a.logf("share: catalog root is not ready: %v", err)
		return ExitFailure
	}
	material := prepared.Registration()
	shareID, shareInstance, pkHash, err := relayRegistrationIdentity(material)
	if err != nil {
		a.logf("share: build relay identity: %v", err)
		return ExitFailure
	}
	var resumeToken v2.ResumeToken
	if _, err := rand.Read(resumeToken[:]); err != nil {
		a.logf("share: generate relay resume credential: %v", err)
		return ExitFailure
	}
	register, err := relayv2.NewFreshRegisterInit(shareID, shareInstance, pkHash, material.Descriptor, resumeToken)
	if err != nil {
		a.logf("share: build relay registration: %v", err)
		return ExitFailure
	}
	connection, err := relayv2.DialSender(ctx, relayv2.SenderConfig{
		RelayBaseURL: request.relayURL, Init: register, SenderPrivateKey: material.SenderPrivateKey,
		Descriptor: material.Descriptor,
	})
	if err != nil {
		if ctx.Err() != nil {
			a.logf("share: interrupted before relay registration")
			return ExitFailure
		}
		a.logf("share: relay registration failed: %v", err)
		return ExitNetwork
	}
	lifecycle, err := newSenderRelayLifecycle(senderRelayLifecycleConfig{
		relayURL: request.relayURL, fresh: register, resumeToken: resumeToken,
		privateKey: material.SenderPrivateKey, initial: connection,
	})
	if err != nil {
		_ = connection.Close()
		a.logf("share: initialize relay lifecycle: %v", err)
		return ExitFailure
	}
	terminalLedger := newShareTerminalLedger()
	factory, err := a.newShareRuntimeFactory(prepared, lifecycle, terminalLedger)
	if err != nil {
		_ = lifecycle.Cleanup(context.Background())
		a.logf("share: initialize session runtime: %v", err)
		return ExitFailure
	}
	if err := a.printShareLink(prepared, request.frontURL, request.splitKey); err != nil {
		stopContext, cancel := context.WithTimeout(context.Background(), shareStopTimeout)
		_ = factory.Stop(stopContext, "Sender stopped")
		cancel()
		a.logf("share: build link: %v", err)
		return ExitUsage
	}
	prepared.StartRootPrefetch()
	a.logf("share: ready; root children warm in the background and deeper descendants remain on demand; press Ctrl-C to stop")

	serveDone := make(chan error, 1)
	go func() { serveDone <- a.serveSessions(ctx, factory, lifecycle) }()
	trigger := shareShutdownServeEnded
	var serveErr error
	select {
	case <-ctx.Done():
		trigger = shareShutdownCallerInterrupted
	case serveErr = <-serveDone:
		trigger = shareTriggerAfterServe(ctx.Err(), serveErr)
	}
	stopContext, cancelStop := context.WithTimeout(context.Background(), shareStopTimeout)
	stopErr := factory.Stop(stopContext, "Sender stopped")
	cancelStop()
	if trigger == shareShutdownCallerInterrupted && serveErr == nil {
		serveErr = awaitInterruptedShareServe(serveDone, shareServeJoinTimeout)
	}
	settlement := settleShareLifecycle(
		trigger,
		ctx.Err(),
		serveErr,
		stopErr,
		terminalLedger.Snapshot(),
	)
	a.logf(
		"share: shutdown share_id=%x trigger=%s serve=%s stop=%s terminal_observations=%d terminal_sessions=%d terminal_delivered_sessions=%d terminal_retired_sessions=%d terminal_failed_sessions=%d terminal_accepted_failed_lanes=%d decision=%s",
		shareID[:],
		settlement.trigger,
		settlement.serve.outcome,
		settlement.stop.outcome,
		settlement.terminals.observations,
		settlement.terminals.sessions,
		settlement.terminals.deliveredSessions,
		settlement.terminals.naturallyRetiredSessions,
		settlement.terminals.failedSessions,
		settlement.terminals.acceptedFailedLanes,
		settlement.decision,
	)
	if err := settlement.Err(); err != nil {
		a.logf("share: stopped with an error: %v", err)
		return ExitNetwork
	}
	if trigger == shareShutdownCallerInterrupted {
		a.logf("share: stopped")
	}
	return ExitOK
}

func (a *App) newShareRuntimeFactory(
	prepared *liveshare.PreparedSender,
	lifecycle *senderRelayLifecycle,
	terminalLedger *shareTerminalLedger,
) (*sessionruntime.SenderFactory, error) {
	peers, err := v2peer.NewFactory(v2peer.Config{
		Configuration: v2peer.DefaultConfiguration(),
		OnError: func(error) {
			a.logf("share: direct peer lane failed; relay service remains available")
		},
	})
	if err != nil {
		return nil, fmt.Errorf("initialize direct peer connectivity: %w", err)
	}
	return prepared.NewRuntimeFactory(liveshare.RuntimeFactoryConfig{
		TerminalConnectivity: lifecycle,
		PeerHandlers:         peers,
		TerminalObserver: sessionruntime.SenderTerminalObserverFunc(
			func(observation sessionruntime.SenderTerminalObservation) {
				terminalLedger.ObserveSenderTerminal(observation)
				a.logf(
					"share: sender terminal session_id=%x lane_id=%d lane_epoch=%d settled=%t transport=%s outcome=%s decision=%s",
					observation.ProtocolSessionID.Bytes(),
					observation.Lane.ID,
					observation.Lane.Epoch,
					observation.Settled,
					observation.TransportDisposition,
					observation.Outcome,
					observation.Decision,
				)
			},
		),
	})
}

func (a *App) parseShareRequest(args []string) (shareRequest, int) {
	flags := a.newFlagSet("share")
	relayURL := flags.String("relay", DefaultRelayURL, "relay server base URL")
	blockSize := flags.Int64("block-size", 0, "file-local block size in bytes; 0 uses 1 MiB")
	splitKey := flags.Bool("split-key", false, "print a bare link and separate key string")
	frontURL := flags.String("front-url", DefaultFrontURL, "frontend base URL embedded in the link")
	paths, err := parseInterleaved(flags, args)
	if err != nil {
		return shareRequest{}, ExitUsage
	}
	if len(paths) == 0 || *relayURL == "" || *frontURL == "" {
		a.logf("share: at least one path, a relay URL, and a frontend URL are required")
		return shareRequest{}, ExitUsage
	}
	chunkSize := int64(catalog.DefaultChunkSize)
	if *blockSize != 0 {
		chunkSize = *blockSize
	}
	if chunkSize < 0 || chunkSize > math.MaxUint32 {
		a.logf("share: block size is outside the suite-02 range")
		return shareRequest{}, ExitUsage
	}
	return shareRequest{
		paths: paths, relayURL: *relayURL, frontURL: *frontURL, chunkSize: uint32(chunkSize), splitKey: *splitKey,
	}, ExitOK
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

func (a *App) printShareLink(sender *liveshare.PreparedSender, frontURL string, split bool) error {
	capability := sender.Capability()
	if split {
		bare, key, err := capability.SplitURL(frontURL)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(a.Stdout, "Bare link: %s\nKey: %s\n", bare, key)
		return err
	}
	full, err := capability.URL(frontURL)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.Stdout, "Link: %s\n", full)
	return err
}

func (a *App) serveSessions(
	ctx context.Context,
	factory *sessionruntime.SenderFactory,
	lifecycle *senderRelayLifecycle,
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
				if ctx.Err() == nil {
					a.logf("share: rejected receiver session: %v", err)
				}
				return
			}
			if admission.Kind == sessionruntime.SenderChannelAttachedLane {
				return
			}
			runtime := admission.Session
			if runtime == nil {
				_ = channel.Close()
				a.logf("share: rejected receiver session: missing admitted runtime")
				return
			}
			<-runtime.Done()
			if err := runtime.Err(); err != nil && ctx.Err() == nil {
				a.logf("share: receiver session ended: %v", err)
			}
			runtime.Close()
		}()
	}
}
