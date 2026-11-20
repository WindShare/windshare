package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

func verifyBudgetRecovery(binary, database string, settings json.RawMessage) {
	process := startNode(binary, database, "127.0.0.1:0", settings)
	defer func() { process.kill() }()
	expectAnonymousBackoff(process.endpoint)
	f := newFixture()
	sender := registerRuntime(process.endpoint, f, false)
	address := process.address
	process.kill()
	_ = sender.Close()

	// Both RESUME and JOIN omit the owner bearer. Their allowance derives from
	// the persisted sender attribution, including the first write after restart.
	process = startNode(binary, database, address, settings)
	sender = registerRuntime(process.endpoint, f, true)
	defer sender.Close()
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	pair := joinRuntime(ctx, process.endpoint, f, sender)
	pair.close()
	stopRuntime(process.endpoint, f)
	expectAnonymousBackoff(process.endpoint)
	process.kill()

	offline := map[string]any{"services": map[string]bool{"relay": false, "stun": true}, "relay_budget": nil}
	process = startNode(binary, database+".offline", address, offline)
	expectAnonymousBackoff(process.endpoint)
}

func expectAnonymousBackoff(endpoint string) {
	f := newFixture()
	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()
	sender, err := relayv2.DialSender(ctx, relayv2.SenderConfig{
		RelayBaseURL: strings.TrimSuffix(endpoint, v2.WebSocketPath), Init: f.init.Registration,
		SenderPrivateKey: f.key, Descriptor: f.descriptor, ResumeToken: f.token,
		Dial: relayv2.DialOptions{ReceiveCapacity: f.init.ReceiveCapacity},
	})
	if sender != nil {
		_ = sender.Close()
	}
	_, unsent := errors.AsType[*relayv2.RegistrationNotSentError](err)
	upgrade, ok := errors.AsType[*relayv2.WebSocketDialError](err)
	require(unsent && ok && upgrade.StatusCode == http.StatusServiceUnavailable && upgrade.RetryAfter == 30*time.Second,
		fmt.Sprintf("expected unsent registration and HTTP 503 with 30s Retry-After, got %v", err))
}
