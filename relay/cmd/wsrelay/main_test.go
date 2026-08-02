package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/testrun"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func TestRunServesOnlyV2AndShutsDownGracefully(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"-listen", "127.0.0.1:0", "-state-dir", t.TempDir(),
		}, func(address net.Addr) error {
			ready <- address
			return nil
		}, t.Logf)
	}()
	var address net.Addr
	select {
	case address = <-ready:
	case err := <-done:
		t.Fatalf("run exited before ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("relay did not become ready")
	}

	response, err := http.Get(fmt.Sprintf("http://%s/healthz", address))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || response.StatusCode != http.StatusOK || string(body) != "ok\n" {
		t.Fatalf("health response = %d %q, read=%v close=%v", response.StatusCode, body, readErr, closeErr)
	}
	response, err = http.Get(fmt.Sprintf("http://%s/v1/config", address))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("retired v1 route status = %d", response.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("graceful shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("graceful shutdown did not complete")
	}
}

func TestRelayReadyReporterEmitsActualAddressAsStructuredTrace(t *testing.T) {
	operation, err := testrun.NewOperation("run-1", "operation-1", "relay-readiness")
	if err != nil {
		t.Fatal(err)
	}
	environment, err := operation.ChildEnvironment(nil)
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		name, value, _ := strings.Cut(entry, "=")
		values[name] = value
	}
	var openedIdentity testrun.Identity
	sink := &recordingRelayReadySink{}
	reporter, err := relayReadyReporter(func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}, func(identity testrun.Identity) (relayReadySink, error) {
		openedIdentity = identity
		return sink, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if reporter == nil {
		t.Fatal("correlated child did not create a ready reporter")
	}
	if err := reporter(stringAddress("127.0.0.1:49231")); err != nil {
		t.Fatal(err)
	}
	var payload testrun.ListenerReadyContext
	payloadErr := json.Unmarshal(sink.event.Payload, &payload)
	if openedIdentity.RunID != operation.RunID() || openedIdentity.OperationID != operation.ID() ||
		openedIdentity.Scenario != operation.Scenario() || sink.event.Identity != openedIdentity ||
		sink.event.Component != string(relayTraceComponent) || sink.event.Milestone != testrun.ListenerReadyMilestone ||
		sink.event.Outcome != string(testrun.OutcomeSucceeded) || !sink.closed || payloadErr != nil ||
		payload.Address != "127.0.0.1:49231" {
		t.Fatalf("ready sink = identity=%+v sink=%+v", openedIdentity, sink)
	}
}

func TestRelayReadyReporterIsOptInAndRejectsPartialContext(t *testing.T) {
	reporter, err := relayReadyReporter(func(string) (string, bool) { return "", false }, nil)
	if err != nil || reporter != nil {
		t.Fatalf("ordinary CLI reporter present = %t, err = %v", reporter != nil, err)
	}
	_, err = relayReadyReporter(func(name string) (string, bool) {
		if name == testrun.RunIDEnvironment {
			return "run-1", true
		}
		return "", false
	}, nil)
	if err == nil {
		t.Fatal("partial correlation context was accepted")
	}
}

type recordingRelayReadySink struct {
	event  testrun.Event
	closed bool
}

func (sink *recordingRelayReadySink) WriteEvent(event testrun.Event) error {
	sink.event = event
	return nil
}

func (sink *recordingRelayReadySink) Close() error {
	sink.closed = true
	return nil
}

func TestRunFailsClosedWhenReadinessCannotBePublished(t *testing.T) {
	publishFailure := errors.New("trace sink unavailable")
	err := run(
		context.Background(),
		[]string{"-listen", "127.0.0.1:0", "-state-dir", t.TempDir()},
		func(net.Addr) error { return publishFailure },
		t.Logf,
	)
	if !errors.Is(err, publishFailure) {
		t.Fatalf("readiness failure = %v", err)
	}
}

func TestServerPolicyBuildsHardenedServer(t *testing.T) {
	policy := serverPolicy{
		readHeaderTimeout: 2 * time.Second, readTimeout: 3 * time.Second,
		idleTimeout: 4 * time.Second, maximumHeader: 1234,
	}
	if err := policy.validate(); err != nil {
		t.Fatal(err)
	}
	server := policy.newServer(http.NewServeMux())
	if server.ReadHeaderTimeout != policy.readHeaderTimeout || server.ReadTimeout != policy.readTimeout ||
		server.IdleTimeout != policy.idleTimeout || server.MaxHeaderBytes != policy.maximumHeader {
		t.Fatalf("server = %+v", server)
	}
}

func TestPublicRelayEndpointUsesActualListenerAndConfiguredIdentity(t *testing.T) {
	derived, err := publicRelayEndpoint("", stringAddress("127.0.0.1:49231"))
	if err != nil {
		t.Fatal(err)
	}
	if derived.DialURL != "ws://127.0.0.1:49231/v2/ws" {
		t.Fatalf("derived endpoint = %+v", derived)
	}
	configured, err := publicRelayEndpoint("https://Relay.Example:443/base?token=one", stringAddress("127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	if configured.DialURL != "wss://relay.example/base/v2/ws?token=one" ||
		configured.IdentityURL != "wss://relay.example/base/v2/ws" {
		t.Fatalf("configured endpoint = %+v", configured)
	}
}

func TestResolveStateDirectorySeparatesRelayIdentities(t *testing.T) {
	first := v2.RelayIdentity{1}
	second := v2.RelayIdentity{2}
	left, err := resolveStateDirectory("", first)
	if err != nil {
		t.Fatal(err)
	}
	right, err := resolveStateDirectory("", second)
	if err != nil {
		t.Fatal(err)
	}
	if left == right || filepath.Base(left) == filepath.Base(right) {
		t.Fatalf("identity directories collided: %q %q", left, right)
	}
	explicit := filepath.Join(t.TempDir(), "state")
	resolved, err := resolveStateDirectory(explicit, first)
	if err != nil || !filepath.IsAbs(resolved) {
		t.Fatalf("explicit state directory = %q, %v", resolved, err)
	}
}

func TestRunRejectsInvalidAndRetiredFlags(t *testing.T) {
	tests := [][]string{
		{"-definitely-not-a-flag"},
		{"-listen", "999.999.999.999:1"},
		{"-listen", "127.0.0.1:0", "-relay-base-url", "not a URL"},
		{"-listen", "127.0.0.1:0", "-max-routes", "0"},
		{"-listen", "127.0.0.1:0", "-max-sessions", "2", "-max-sessions-per-share", "3"},
		{"-listen", "127.0.0.1:0", "-max-connections", "1", "-max-connections-per-source", "2"},
		{"-listen", "127.0.0.1:0", "-http-read-header-timeout", "5s", "-http-read-timeout", "1s"},
		{"-listen", "127.0.0.1:0", "-max-manifest-bytes", "1"},
		{"-listen", "127.0.0.1:0", "-sender-grace", "1s"},
	}
	for _, arguments := range tests {
		if err := run(context.Background(), arguments, nil, t.Logf); err == nil {
			t.Fatalf("invalid policy was accepted: %v", arguments)
		}
	}
}

type stringAddress string

func (address stringAddress) Network() string { return "tcp" }
func (address stringAddress) String() string  { return string(address) }

func TestProductionSourceContainsNoV1RouteOrManifestFlag(t *testing.T) {
	for _, retired := range []string{"/v1", "max-manifest", "sender-grace"} {
		if strings.Contains(strings.ToLower(sourceText(t)), retired) {
			t.Fatalf("production relay source still contains %q", retired)
		}
	}
}

func sourceText(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
