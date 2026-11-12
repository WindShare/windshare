package desktop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/host/desktop/logstore"
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
	log            *logstore.Recorder
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
	if err != nil {
		return nil, err
	}
	directory := filepath.Join(cache, "WindShare", "logs")
	log, err := logstore.Open(directory, sessionID)
	if err != nil {
		return nil, err
	}
	application, err := engine.New(engine.Config{})
	if err != nil {
		_ = log.Close()
		return nil, err
	}
	host := &Host{log: log, diagnosticDone: make(chan struct{})}
	deps := dependencies{
		start: func(request engine.ShareRequest) (shareTask, error) {
			return application.StartShare(context.Background(), request)
		},
		close: application.Close, load: relayconfig.Load, log: log.Record,
		configure: application.ConfigureTURN,
	}
	if os.Getenv("WINDSHARE_TRACE") == "1" {
		host.diagnostics, err = diagnostics.NewStream(diagnosticCapacity, nil)
		if err != nil {
			_ = application.Close(context.Background())
			_ = log.Close()
			return nil, err
		}
		deps.diagnostics = host.diagnostics.Sink()
		go host.recordDiagnostics()
	} else {
		close(host.diagnosticDone)
	}
	host.app = newApplication(sessionID, deps)
	host.server, err = listen(host.app, token)
	if err != nil {
		_ = host.closeEngine()
		return nil, err
	}
	host.Endpoint = Endpoint{SessionID: sessionID, Address: host.server.address, Token: token, LogDirectory: directory}
	log.Record("host.listening", map[string]string{"session_id": sessionID, "address": host.server.address})
	return host, nil
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

func (h *Host) closeEngine() error {
	err := h.app.close()
	if h.diagnostics != nil {
		h.diagnostics.Close()
	}
	<-h.diagnosticDone
	return errors.Join(err, h.log.Close())
}

// Close returns only after the control workers, engine, and log writer join.
func (h *Host) Close() error {
	h.closeOnce.Do(func() {
		h.server.close()
		h.closeErr = h.closeEngine()
	})
	return h.closeErr
}
