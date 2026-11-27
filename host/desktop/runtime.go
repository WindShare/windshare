package desktop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/builtinnetwork"
	"github.com/windshare/windshare/engine/networkconfig"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relaysetup"
	"github.com/windshare/windshare/internal/diaglog"
	"github.com/windshare/windshare/internal/powerinhibit"
)

const (
	identityBytes             = 32
	sessionIdentityCharacters = 16
	diagnosticCapacity        = 1024
)

type Endpoint struct {
	SessionID    string `json:"session_id"`
	Address      string `json:"address"`
	Token        string `json:"token"`
	LogDirectory string `json:"log_directory"`
}

// Host owns exactly one engine. Endpoint readiness only proves that control is
// listening; create/query report configuration and share startup separately.
type Host struct {
	Endpoint       Endpoint
	app            *Application
	server         *server
	log            *diaglog.Recorder
	diagnostics    *diagnostics.Stream
	diagnosticDone chan struct{}
	closeOnce      sync.Once
	closeErr       error
}

func Start() (*Host, error) {
	var entropy [identityBytes]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(entropy[:])
	sessionID := token[:sessionIdentityCharacters]
	cache, err := os.UserCacheDir()
	directory := filepath.Join(cache, "WindShare", "logs")
	var log *diaglog.Recorder
	if err == nil {
		log, err = diaglog.OpenRotating(directory, sessionID)
	}
	logAvailable := err == nil
	if !logAvailable {
		_, _ = fmt.Fprintf(os.Stderr, "host.log_unavailable session_id=%s error=%v\n", sessionID, err)
		log = diaglog.Disabled(sessionID, err)
	}
	configPath, err := relayconfig.Path()
	if err != nil {
		_ = log.Close()
		return nil, err
	}
	network, err := builtinnetwork.Config(configPath)
	if err != nil {
		_ = log.Close()
		return nil, err
	}
	host := &Host{log: log, diagnosticDone: make(chan struct{})}
	var trace *senderTrace
	if logAvailable {
		host.diagnostics, err = diagnostics.NewStream(diagnosticCapacity, nil)
		if err != nil {
			_ = log.Close()
			return nil, err
		}
		trace = newSenderTrace(host.diagnostics, os.Getenv("WINDSHARE_TRACE") == "1")
		go host.recordDiagnostics()
	} else {
		directory = ""
		close(host.diagnosticDone)
	}
	communityNotices := make(chan struct{}, 1)
	network.CommunityPaused = func() {
		select {
		case communityNotices <- struct{}{}:
		default:
		}
	}
	store := relayconfig.FileStore{}
	application, err := engine.New(engine.Config{
		Network:        network,
		Diagnostics:    trace.engineDiagnostics(),
		RelaySetup:     relaysetup.Dependencies{Store: relaysetup.FileStore{Settings: store}},
		SleepInhibitor: powerinhibit.New(),
	})
	if err != nil {
		host.stopDiagnostics()
		_ = log.Close()
		return nil, err
	}
	setup := newNativeSetup(store, application)
	deps := dependencies{
		defaultFrontend:       defaultBuiltInFrontend(network),
		networkUnavailability: application.NetworkUnavailability,
		start: func(ctx context.Context, request engine.ShareRequest) (shareTask, error) {
			task, err := application.StartShare(ctx, request)
			if err != nil {
				// Preserve absence before converting the concrete pointer to an interface.
				return nil, err
			}
			return task, nil
		},
		close: application.Close, load: relayconfig.Load, log: log.Record, addDropped: log.AddDropped,
		setup:       setup.run,
		cancelSetup: setup.cancel,
		senderTrace: trace,
	}
	if accounts := application.Accounts(); accounts != nil {
		deps.accounts = accounts
	}
	host.app = newApplication(sessionID, deps)
	host.app.watchCommunityNotices(communityNotices)
	host.server, err = listen(host.app, token)
	if err != nil {
		_ = host.closeEngine()
		return nil, err
	}
	host.Endpoint = Endpoint{SessionID: sessionID, Address: host.server.address, Token: token, LogDirectory: directory}
	log.Record("host.listening", map[string]string{"session_id": sessionID, "address": host.server.address})
	return host, nil
}

// Match networkruntime's built-in fallback order so backup-only custom builds
// remain shareable without requiring an explicit front_url override.
func defaultBuiltInFrontend(network *networkconfig.Config) string {
	if network == nil {
		return ""
	}
	if network.LinkHost != "" {
		return network.LinkHost
	}
	return network.BackupEntry
}

func (h *Host) recordDiagnostics() {
	defer close(h.diagnosticDone)
	for record := range h.diagnostics.Records() {
		h.log.Record("engine.trace", map[string]any{
			"name": record.Name(), "at": record.ObservedAt(), "scope": record.Scope(),
			"fields": record.Payload(), "correlation": record.Correlation(),
		})
	}
}

func (h *Host) stopDiagnostics() {
	if h.diagnostics != nil {
		completion := h.diagnostics.Close()
		h.log.AddDropped(completion.CapacityDropped)
	}
	<-h.diagnosticDone
}

func (h *Host) closeEngine() error {
	err := h.app.close()
	h.stopDiagnostics()
	if logErr := h.log.Close(); logErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "host.log_closed session_id=%s error=%v\n", h.Endpoint.SessionID, logErr)
	}
	return err
}

// Close returns only after the control workers, engine, and log writer join.
func (h *Host) Close() error {
	h.closeOnce.Do(func() {
		h.server.close()
		h.closeErr = h.closeEngine()
	})
	return h.closeErr
}
