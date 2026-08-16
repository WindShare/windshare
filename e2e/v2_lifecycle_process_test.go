package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/internal/testloopback"
	"github.com/windshare/windshare/internal/testoutputroot"
	"github.com/windshare/windshare/internal/testrun"
)

const (
	v2CriticalRelayTransferScenario = "v2-critical-relay-transfer"
	v2SenderReconnectScenario       = "v2-sender-relay-reconnect"
	v2ExplicitStopScenario          = "v2-explicit-stop-tombstone"
	v2CriticalPayload               = "critical sender-relay-receiver\n"

	v2SenderRelayRecoveryMilestone  = "sender_relay_recovery"
	v2SenderDirectLaneMilestone     = "sender_direct_lane"
	v2SenderStopMilestone           = "sender_stop"
	v2ReceiverDirectLaneMilestone   = "receiver_direct_lane"
	v2ReceiverRelayContentMilestone = "receiver_relay_content"
	v2ReceiverJoinStoppedMilestone  = "receiver_join_stopped"
)

func TestCriticalSenderRelayReceiver(t *testing.T) {
	requireV2ProcessScenario(t)
	scenario := startV2Scenario(t, v2CriticalRelayTransferScenario)
	binaries := loadE2EBinaries(t)
	relay := startV2Process(
		t, scenario, v2RelayComponent, binaries.relay,
		"-listen", "127.0.0.1:0", "-state-dir", filepath.Join(t.TempDir(), "relay-state"),
	)
	relayURL := "ws://" + waitV2RelayReady(t, relay)

	payload := []byte(v2CriticalPayload)
	source := filepath.Join(t.TempDir(), "critical.txt")
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	share := startV2Process(
		t, scenario, v2WindShareShareComponent, binaries.windshare,
		"share", source, "--relay", relayURL,
	)
	shareLink := waitV2Match(t, share, regexp.MustCompile(`(?m)^Link: (\S+)$`), share.stdout)
	output := testoutputroot.New(t).RootPath
	receiver := startV2Process(
		t, scenario, v2WindShareGetComponent, binaries.windshare,
		"get", shareLink, "-o", output, "--connectivity", "relay-only",
	)
	if err := receiver.wait(t); err != nil {
		t.Fatalf(
			"relay-only receiver failed: %v; stdout=%q stderr=%q",
			err, receiver.stdout.String(), receiver.stderr.String(),
		)
	}
	assertV2File(t, filepath.Join(output, filepath.Base(source)), payload)
	events := drainV2ProcessTraces(t, receiver)
	requireV2EventCount(t, events, v2ReceiverRelayContentMilestone, testrun.OutcomeSucceeded, 1)
	requireV2EventCount(t, events, v2ReceiverDirectLaneMilestone, testrun.OutcomeSucceeded, 0)

	p2pOutput := testoutputroot.New(t).RootPath
	p2pReceiver := startV2Process(
		t, scenario, v2WindShareGetComponent, binaries.windshare,
		"get", shareLink, "-o", p2pOutput, "--connectivity", "p2p-only",
	)
	if err := p2pReceiver.wait(t); err != nil {
		t.Fatalf(
			"p2p-only receiver failed: %v; stdout=%q stderr=%q",
			err, p2pReceiver.stdout.String(), p2pReceiver.stderr.String(),
		)
	}
	assertV2File(t, filepath.Join(p2pOutput, filepath.Base(source)), payload)
	p2pEvents := drainV2ProcessTraces(t, p2pReceiver)
	requireV2EventCount(t, p2pEvents, v2ReceiverRelayContentMilestone, testrun.OutcomeSucceeded, 0)
	requireV2EventCount(t, p2pEvents, v2ReceiverDirectLaneMilestone, testrun.OutcomeSucceeded, 1)
	scenario.requireSuccess(t)
}

func TestLongV2ProcessSenderReconnectsAfterRelayPathRestoration(t *testing.T) {
	requireV2ProcessScenario(t)
	scenario := startV2Scenario(t, v2SenderReconnectScenario)
	binaries := loadE2EBinaries(t)
	proxy := startRelayPauseProxy(t, scenario)
	relay := startLifecycleRelay(t, scenario, binaries, proxy, filepath.Join(t.TempDir(), "relay-state"))
	_ = relay

	payload := bytes.Repeat([]byte("sender-reconnected\n"), 8192)
	source := filepath.Join(t.TempDir(), "after-reconnect.bin")
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	share := startV2Process(
		t, scenario, v2WindShareShareComponent, binaries.windshare,
		"share", source, "--relay", proxy.BaseURL(),
	)
	shareLink := waitV2Match(t, share, regexp.MustCompile(`(?m)^Link: (\S+)$`), share.stdout)
	pauseContext, cancelPause := context.WithTimeout(context.Background(), v2ProcessTerminationGrace)
	if err := scenario.observe(v2RelayProxyPauseMilestone, nil, func() error {
		return proxy.Pause(pauseContext)
	}); err != nil {
		cancelPause()
		t.Fatal(err)
	}
	cancelPause()
	waitV2ProcessTrace(t, share, v2SenderRelayRecoveryMilestone, testrun.OutcomeStarted)
	if err := scenario.observe(v2RelayProxyResumeMilestone, nil, func() error {
		proxy.Resume()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	waitV2ProcessTrace(t, share, v2SenderRelayRecoveryMilestone, testrun.OutcomeSucceeded)

	output := testoutputroot.New(t).RootPath
	receiver := startV2Process(
		t, scenario, v2WindShareGetComponent, binaries.windshare,
		"get", shareLink, "-o", output, "--connectivity", "relay-only",
	)
	if err := receiver.wait(t); err != nil {
		t.Fatalf(
			"receiver after sender reconnect failed: %v; receiver stderr=%q sender stderr=%q",
			err, receiver.stderr.String(), share.stderr.String(),
		)
	}
	assertV2File(t, filepath.Join(output, filepath.Base(source)), payload)
	scenario.requireSuccess(t)
}

func TestLongV2ProcessExplicitStopPublishesDurableRelayTombstone(t *testing.T) {
	requireV2ProcessScenario(t)
	scenario := startV2Scenario(t, v2ExplicitStopScenario)
	binaries := loadE2EBinaries(t)
	proxy := startRelayPauseProxy(t, scenario)
	stateDirectory := filepath.Join(t.TempDir(), "relay-state")
	firstRelay := startLifecycleRelay(t, scenario, binaries, proxy, stateDirectory)

	source := filepath.Join(t.TempDir(), "stopped-share.txt")
	if err := os.WriteFile(source, []byte("must never be available after STOP"), 0o600); err != nil {
		t.Fatal(err)
	}
	share := startV2Process(
		t, scenario, v2WindShareShareComponent, binaries.windshare,
		"share", source, "--relay", proxy.BaseURL(),
	)
	shareLink := waitV2Match(t, share, regexp.MustCompile(`(?m)^Link: (\S+)$`), share.stdout)
	share.interrupt(t)
	waitV2ProcessTrace(t, share, v2SenderStopMilestone, testrun.OutcomeSucceeded)
	if err := share.wait(t); err != nil {
		t.Fatalf("sender failed while committing STOP: %v; stderr=%q", err, share.stderr.String())
	}

	// Reloading the same relay state behind the stable proxy proves the verdict is
	// a durable route tombstone, not merely a closed in-memory sender connection.
	pauseContext, cancelPause := context.WithTimeout(context.Background(), v2ProcessTerminationGrace)
	if err := scenario.observe(v2RelayProxyPauseMilestone, nil, func() error {
		return proxy.Pause(pauseContext)
	}); err != nil {
		cancelPause()
		t.Fatal(err)
	}
	cancelPause()
	firstRelay.stop(t)
	secondRelay := startV2RelayBehindPausedProxy(t, scenario, binaries, proxy, stateDirectory)
	_ = secondRelay
	if err := scenario.observe(v2RelayProxyResumeMilestone, nil, func() error {
		proxy.Resume()
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		output := testoutputroot.New(t).RootPath
		receiver := startV2Process(
			t, scenario, v2WindShareGetComponent, binaries.windshare,
			"get", shareLink, "-o", output, "--connectivity", "relay-only",
		)
		if err := receiver.wait(t); err == nil {
			t.Fatalf("post-STOP receiver %d unexpectedly succeeded", attempt)
		}
		events := drainV2ProcessTraces(t, receiver)
		requireV2EventCount(t, events, v2ReceiverJoinStoppedMilestone, testrun.OutcomeFailed, 1)
	}
	scenario.requireSuccess(t)
}

func startLifecycleRelay(
	t *testing.T,
	scenario *v2Scenario,
	binaries e2eBinaries,
	proxy *relayPauseProxy,
	stateDirectory string,
) *v2Process {
	t.Helper()
	relay := startV2RelayBehindPausedProxy(t, scenario, binaries, proxy, stateDirectory)
	if err := scenario.observe(v2RelayProxyResumeMilestone, nil, func() error {
		proxy.Resume()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return relay
}

func startV2RelayBehindPausedProxy(
	t *testing.T,
	scenario *v2Scenario,
	binaries e2eBinaries,
	proxy *relayPauseProxy,
	stateDirectory string,
) *v2Process {
	t.Helper()
	relay := startV2Process(
		t, scenario, v2RelayComponent, binaries.relay,
		"-listen", "127.0.0.1:0",
		"-relay-base-url", proxy.BaseURL(),
		"-state-dir", stateDirectory,
	)
	address := waitV2RelayReady(t, relay)
	if err := scenario.observe(
		v2RelayProxyForwardMilestone,
		nil,
		func() error { return proxy.ForwardTo(address) },
	); err != nil {
		t.Fatal(err)
	}
	return relay
}

func waitV2ProcessTrace(
	t *testing.T,
	process *v2Process,
	milestone string,
	outcome testrun.Outcome,
) {
	t.Helper()
	_ = waitV2ProcessEvent(t, process, milestone, outcome)
}

func waitV2ProcessEvent(
	t *testing.T,
	process *v2Process,
	milestone string,
	outcome testrun.Outcome,
) testrun.Event {
	t.Helper()
	phaseContext := v2ProcessPhaseContext{Component: process.component}
	phase := process.scenario.startPhase(t, v2ProcessEventWaitMilestone, phaseContext)
	ctx, cancel := context.WithTimeout(context.Background(), v2ProcessTimeout)
	defer cancel()
	for {
		event, err := process.owned.Events().Next(ctx)
		if err != nil {
			t.Fatalf(
				"wait for process event %s/%s: %v; stderr=%q",
				milestone, outcome, err, process.stderr.String(),
			)
		}
		validateV2ProcessEvent(t, process, event)
		if event.Milestone == milestone && event.Outcome == string(outcome) {
			process.scenario.succeedPhase(t, phase, phaseContext)
			return event
		}
	}
}

func drainV2ProcessTraces(
	t *testing.T,
	process *v2Process,
) []testrun.Event {
	t.Helper()
	phaseContext := v2ProcessPhaseContext{Component: process.component}
	phase := process.scenario.startPhase(t, v2ProcessEventDrainMilestone, phaseContext)
	ctx, cancel := context.WithTimeout(context.Background(), v2ProcessTimeout)
	defer cancel()
	var events []testrun.Event
	for {
		event, err := process.owned.Events().Next(ctx)
		if errors.Is(err, io.EOF) {
			process.scenario.succeedPhase(t, phase, phaseContext)
			return events
		}
		if err != nil {
			t.Fatalf("drain process events: %v; stderr=%q", err, process.stderr.String())
		}
		validateV2ProcessEvent(t, process, event)
		events = append(events, event)
	}
}

func validateV2ProcessEvent(t *testing.T, process *v2Process, event testrun.Event) {
	t.Helper()
	wantIdentity := process.scenario.operation.EventIdentity()
	if event.Identity != wantIdentity {
		t.Fatalf("process event identity = %#v, want %#v", event.Identity, wantIdentity)
	}
	if event.Component != process.component {
		t.Fatalf("process event component = %q, want %q", event.Component, process.component)
	}
}

func requireV2EventCount(
	t *testing.T,
	events []testrun.Event,
	milestone string,
	outcome testrun.Outcome,
	want int,
) {
	t.Helper()
	got := 0
	for _, event := range events {
		if event.Milestone == milestone && event.Outcome == string(outcome) {
			got++
		}
	}
	if got != want {
		t.Fatalf("event count for %s/%s = %d, want %d; events=%+v", milestone, outcome, got, want, events)
	}
}

// relayPauseProxy keeps one stable public endpoint while tests interrupt and
// restore the sender's real WebSocket path or reload a relay on a new ephemeral
// backend address. It owns every accepted connection and never reserves a port
// for a later listener.
type relayPauseProxy struct {
	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc

	mu                sync.Mutex
	target            string
	paused            bool
	stopping          bool
	connections       map[net.Conn]struct{}
	active            int
	changed           chan struct{}
	firstErr          error
	connectionContext context.Context
	cancelConnections context.CancelFunc

	acceptDone chan struct{}
	closeOnce  sync.Once
	finishOnce sync.Once
	finishErr  error
	downstream atomic.Uint64
}

func startRelayPauseProxy(t *testing.T, scenario *v2Scenario) *relayPauseProxy {
	t.Helper()
	loopback := testloopback.New(t)
	scenario.trace.RequireCleanup(t, "relay pause proxy loopback sockets", func(context.Context) error {
		return loopback.Close()
	})
	listener := loopback.ListenTCP()
	ctx, cancel := context.WithCancel(context.Background())
	connectionContext, cancelConnections := context.WithCancel(ctx)
	proxy := &relayPauseProxy{
		listener: listener, ctx: ctx, cancel: cancel,
		connectionContext: connectionContext, cancelConnections: cancelConnections,
		connections: make(map[net.Conn]struct{}),
		changed:     make(chan struct{}), acceptDone: make(chan struct{}),
	}
	go proxy.accept()
	cleanup := func(ctx context.Context) error {
		return proxy.Close(ctx)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), v2ProcessTerminationGrace)
		defer cancel()
		if err := cleanup(ctx); err != nil {
			t.Errorf("close relay pause proxy: %v", err)
		}
	})
	scenario.trace.RequireCleanup(t, "relay pause proxy", cleanup)
	return proxy
}

func (proxy *relayPauseProxy) BaseURL() string {
	return "ws://" + proxy.listener.Addr().String()
}

func (proxy *relayPauseProxy) ForwardTo(address string) error {
	if err := validateV2RelayAddress(address); err != nil {
		return fmt.Errorf("relay pause proxy target: %w", err)
	}
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	if proxy.stopping {
		return errors.New("relay pause proxy is stopping")
	}
	if proxy.target != "" && !proxy.paused {
		return errors.New("relay pause proxy target can change only while paused")
	}
	proxy.target = address
	proxy.notifyLocked()
	return nil
}

func (proxy *relayPauseProxy) Pause(ctx context.Context) error {
	proxy.mu.Lock()
	if proxy.stopping {
		proxy.mu.Unlock()
		return errors.New("relay pause proxy is stopping")
	}
	if !proxy.paused {
		proxy.paused = true
		proxy.cancelConnections()
	}
	connections := make([]net.Conn, 0, len(proxy.connections))
	for connection := range proxy.connections {
		connections = append(connections, connection)
	}
	proxy.mu.Unlock()
	for _, connection := range connections {
		_ = connection.Close()
	}
	return waitRelayProxyActivities(ctx, proxy.activitySnapshot)
}

func (proxy *relayPauseProxy) Resume() {
	proxy.mu.Lock()
	if proxy.paused && !proxy.stopping {
		proxy.connectionContext, proxy.cancelConnections = context.WithCancel(proxy.ctx)
		proxy.paused = false
	}
	proxy.notifyLocked()
	proxy.mu.Unlock()
}

func (proxy *relayPauseProxy) DownstreamBytes() uint64 { return proxy.downstream.Load() }

func (proxy *relayPauseProxy) Close(ctx context.Context) error {
	proxy.closeOnce.Do(func() {
		proxy.mu.Lock()
		proxy.stopping = true
		proxy.cancelConnections()
		connections := make([]net.Conn, 0, len(proxy.connections))
		for connection := range proxy.connections {
			connections = append(connections, connection)
		}
		proxy.notifyLocked()
		proxy.mu.Unlock()
		proxy.cancel()
		_ = proxy.listener.Close()
		for _, connection := range connections {
			_ = connection.Close()
		}
	})
	proxy.finishOnce.Do(func() {
		proxy.finishErr = waitRelayProxyQuiescence(ctx, proxy.acceptDone, proxy.activitySnapshot)
	})
	return proxy.finishErr
}

func (proxy *relayPauseProxy) accept() {
	defer close(proxy.acceptDone)
	for {
		front, err := proxy.listener.Accept()
		if err != nil {
			proxy.mu.Lock()
			if !proxy.stopping && proxy.firstErr == nil {
				proxy.firstErr = fmt.Errorf("accept relay pause proxy: %w", err)
			}
			proxy.mu.Unlock()
			return
		}
		proxy.mu.Lock()
		if proxy.stopping || proxy.paused || proxy.target == "" {
			proxy.mu.Unlock()
			_ = front.Close()
			continue
		}
		target := proxy.target
		connectionContext := proxy.connectionContext
		proxy.connections[front] = struct{}{}
		proxy.active++
		proxy.notifyLocked()
		proxy.mu.Unlock()
		go proxy.serve(front, target, connectionContext)
	}
}

func (proxy *relayPauseProxy) serve(front net.Conn, target string, connectionContext context.Context) {
	defer proxy.release(front, nil)
	backend, err := (&net.Dialer{}).DialContext(connectionContext, "tcp", target)
	if err != nil {
		return
	}
	if !proxy.retainBackend(backend) {
		_ = backend.Close()
		return
	}
	defer proxy.release(nil, backend)

	results := make(chan error, 2)
	proxy.addActivities(2)
	go func() {
		defer proxy.finishActivity()
		_, copyErr := io.Copy(backend, front)
		results <- copyErr
	}()
	go func() {
		defer proxy.finishActivity()
		_, copyErr := io.Copy(relayLifecycleDownstreamWriter{target: front, bytes: &proxy.downstream}, backend)
		results <- copyErr
	}()
	select {
	case <-results:
	case <-connectionContext.Done():
	}
	_ = front.Close()
	_ = backend.Close()
	select {
	case <-results:
	case <-connectionContext.Done():
	}
}

func (proxy *relayPauseProxy) retainBackend(connection net.Conn) bool {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	if proxy.stopping || proxy.paused {
		return false
	}
	proxy.connections[connection] = struct{}{}
	return true
}

func (proxy *relayPauseProxy) release(front, backend net.Conn) {
	proxy.mu.Lock()
	if front != nil {
		delete(proxy.connections, front)
		proxy.active--
	}
	if backend != nil {
		delete(proxy.connections, backend)
	}
	proxy.notifyLocked()
	proxy.mu.Unlock()
	if front != nil {
		_ = front.Close()
	}
	if backend != nil {
		_ = backend.Close()
	}
}

func (proxy *relayPauseProxy) addActivities(count int) {
	proxy.mu.Lock()
	proxy.active += count
	proxy.notifyLocked()
	proxy.mu.Unlock()
}

func (proxy *relayPauseProxy) finishActivity() {
	proxy.mu.Lock()
	proxy.active--
	proxy.notifyLocked()
	proxy.mu.Unlock()
}

func (proxy *relayPauseProxy) activitySnapshot() (int, <-chan struct{}, error) {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	return proxy.active, proxy.changed, proxy.firstErr
}

func (proxy *relayPauseProxy) notifyLocked() {
	close(proxy.changed)
	proxy.changed = make(chan struct{})
}

type relayLifecycleDownstreamWriter struct {
	target io.Writer
	bytes  *atomic.Uint64
}

func (writer relayLifecycleDownstreamWriter) Write(value []byte) (int, error) {
	written, err := writer.target.Write(value)
	writer.bytes.Add(uint64(written))
	return written, err
}
