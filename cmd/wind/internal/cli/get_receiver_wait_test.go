package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	sendersession "github.com/windshare/windshare/core/session/sendersession"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/internal/testoutputroot"
	"github.com/windshare/windshare/osfs"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

type getRecoveryClock struct{ elapsed atomic.Int64 }

func (clock *getRecoveryClock) Now() time.Time { return time.Unix(1, clock.elapsed.Load()) }
func (clock *getRecoveryClock) Wait(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clock.elapsed.Add(int64(delay))
	return nil
}

func TestGetWaitTimeoutRejectsInvalidDuration(t *testing.T) {
	for _, value := range []string{"-1s", "invalid"} {
		errorsOutput := &lockedTestBuffer{}
		app := &App{Stderr: errorsOutput, Stdout: &lockedTestBuffer{}, Stdin: strings.NewReader("")}
		if code := app.Run(context.Background(), []string{"get", "unused-link", "--wait-timeout", value}); code != ExitUsage {
			t.Fatal(value, code, errorsOutput.String())
		}
	}
}

func TestGetFirstJoinWaitTimeoutAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "caller"}[canceled], func(t *testing.T) {
			clock := &getRecoveryClock{}
			dials := atomic.Int32{}
			output := testoutputroot.New(t)
			errorsOutput := &lockedTestBuffer{}
			app := &App{
				Stdout: &lockedTestBuffer{}, Stderr: errorsOutput, Stdin: strings.NewReader(""),
				receiverRecoveryOptions: engine.ReceiveRecoverySettings{Clock: clock, Jitter: func(delay time.Duration) time.Duration { return delay }},
				receiverDial: func(context.Context, engine.ReceiveDialRequest) (engine.ReceiveConnection, error) {
					dials.Add(1)
					return nil, &relayv2.RelayError{Code: v2.ErrorNotFound}
				},
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if canceled {
				cancel()
			}
			code := app.Run(ctx, []string{"get", validGetTestLink(t), "-o", output.RootPath, "--wait-timeout", "2s"})
			expected := ExitNetwork
			if canceled {
				expected = ExitFailure
			}
			if code != expected {
				t.Fatalf("exit=%d stderr=%s", code, errorsOutput.String())
			}
			if canceled {
				if dials.Load() != 0 {
					t.Fatal("dialed after caller cancellation")
				}
			} else {
				if clock.Now() != time.Unix(3, 0) || dials.Load() < 2 {
					t.Fatal(clock.Now(), dials.Load(), errorsOutput.String())
				}
				if !strings.Contains(errorsOutput.String(), "not currently available") || !strings.Contains(errorsOutput.String(), "original link") {
					t.Fatal(errorsOutput.String())
				}
			}
		})
	}
}

func validGetTestLink(t *testing.T) string {
	t.Helper()
	capability := newSemanticCapability(t, "wss://relay.example")
	encoded, err := capability.URL("https://app.example")
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestGetSessionReplacementPreservesRecoveryFailureEndToEnd(t *testing.T) {
	stages := []struct {
		name              string
		dropAfterFrames   int32
		expectTransferJob bool
	}{
		// Handshake sends ServerHello (1 frame); closing before the second frame
		// forces replacement during ResolveOrdinaryOutputShape (preparation).
		{name: "preparation", dropAfterFrames: 1, expectTransferJob: false},
		// Allowing catalog + revision open responses lets TransferJob start, then
		// dropping during content delivery forces mid-transfer replacement.
		{name: "mid-transfer", dropAfterFrames: 4, expectTransferJob: true},
	}
	cases := []struct {
		name        string
		waitTimeout string
		replaceErr  error
		wantReason  string
		wantAdvance bool
	}{
		{
			name:        "wait timeout expiry",
			waitTimeout: "2s",
			replaceErr:  &relayv2.RelayError{Code: v2.ErrorNotFound},
			wantReason:  "The operation timed out.",
			wantAdvance: true,
		},
		{
			name:       "relay access denied",
			replaceErr: &relayv2.RelayError{Code: v2.ErrorAccessDenied},
			wantReason: "Relay access was denied.",
		},
		{
			name:       "share stopped",
			replaceErr: &relayv2.RelayError{Code: v2.ErrorStopped},
			wantReason: "This share has stopped.",
		},
	}

	for _, stage := range stages {
		for _, tc := range cases {
			t.Run(stage.name+"/"+tc.name, func(t *testing.T) {
				sender, factory := newInMemoryGetSender(t)
				encoded, err := sender.Capability().URL("https://app.example")
				if err != nil {
					t.Fatal(err)
				}
				clock := &getRecoveryClock{}
				output := testoutputroot.New(t)
				errorsOutput := &lockedTestBuffer{}
				var dials atomic.Int32
				var accepted sync.WaitGroup
				t.Cleanup(accepted.Wait)

				app := &App{
					Stdout: &lockedTestBuffer{}, Stderr: errorsOutput, Stdin: strings.NewReader(""),
					receiverRecoveryOptions: engine.ReceiveRecoverySettings{
						Clock:  clock,
						Jitter: func(delay time.Duration) time.Duration { return delay },
					},
					receiverDial: func(ctx context.Context, _ engine.ReceiveDialRequest) (engine.ReceiveConnection, error) {
						if dials.Add(1) > 1 {
							return nil, tc.replaceErr
						}
						senderWire, receiverWire := newTestFramePipe()
						dropWire := &dropAfterSendChannel{
							Channel:         senderWire,
							closePeer:       receiverWire.Close,
							dropAfterFrames: stage.dropAfterFrames,
						}
						accepted.Add(1)
						go func() {
							defer accepted.Done()
							if runtime, acceptErr := factory.Accept(ctx, dropWire); acceptErr == nil && runtime != nil {
								<-runtime.Done()
								runtime.Close()
							}
						}()
						return &inMemoryReceiveConnection{
							descriptor: sender.Registration().Descriptor,
							channel:    receiverWire,
						}, nil
					},
				}

				args := []string{"get", encoded, "-o", output.RootPath, "--connectivity", "relay-only"}
				if tc.waitTimeout != "" {
					args = append(args, "--wait-timeout", tc.waitTimeout)
				}
				code := app.Run(t.Context(), args)
				if code != ExitNetwork {
					t.Fatalf("exit=%d want=%d stderr=%q", code, ExitNetwork, errorsOutput.String())
				}
				if dials.Load() < 2 {
					t.Fatalf("dials=%d, expected replacement dial attempt; stderr=%q", dials.Load(), errorsOutput.String())
				}
				got := errorsOutput.String()
				if stage.expectTransferJob {
					if !strings.Contains(got, "Reason: "+tc.wantReason) {
						t.Fatalf("mid-transfer stderr = %q, want transfer result reason %q", got, tc.wantReason)
					}
				} else {
					if !strings.Contains(got, "Error: "+tc.wantReason) {
						t.Fatalf("preparation stderr = %q, want command error %q", got, tc.wantReason)
					}
				}
				if strings.Contains(got, "The transfer session failed.") {
					t.Fatalf("replacement failure collapsed into generic session_failed: %q", got)
				}
				if tc.wantAdvance && clock.Now() != time.Unix(3, 0) {
					t.Fatalf("clock = %v, want 2s elapsed", clock.Now())
				}
			})
		}
	}
}

func newInMemoryGetSender(t *testing.T) (*liveshare.PreparedSender, *sendersession.Factory) {
	t.Helper()
	pool, err := content.NewHandlePool(content.DefaultHandlePoolConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := pool.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	sourcePath := filepath.Join(t.TempDir(), "payload.bin")
	payload := bytes.Repeat([]byte("windshare-replacement-failure-test-"), 2000)
	if err := os.WriteFile(sourcePath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	catalogBudget, err := catalog.NewBudgetAccount("cli-test", catalog.DefaultProcessBudgetLimits())
	if err != nil {
		t.Fatal(err)
	}
	cacheBudget, err := contentflow.NewProcessCacheBudget(uint64(16) << 20)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := liveshare.PrepareSender(t.Context(), liveshare.SenderConfig{
		Relays:           []string{"wss://relay.example"},
		ChunkSize:        catalog.MinChunkSize,
		RevisionCapacity: pool,
		CatalogBudget:    catalogBudget,
		CacheBudget:      cacheBudget,
		Source: liveshare.FileSourceFactoryFunc(func(ctx context.Context, src liveshare.FileSourceContext) (liveshare.FileSource, error) {
			return osfs.NewSelectedFileSource(ctx, osfs.SelectedCatalogSourceConfig{
				Paths:         []string{sourcePath},
				SyntheticRoot: src.SyntheticRoot,
				Identities:    osfs.CatalogIdentitySourceFunc(src.NewIdentity),
			})
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := prepared.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	factory, err := prepared.NewRuntimeFactory(liveshare.RuntimeFactoryConfig{
		TerminalConnectivity: noopSenderTerminal{},
		PeerHandlers: sendersession.PeerHandlerFactoryFunc(func(sendersession.PeerSession) (sendersession.PeerHandler, error) {
			return noopSenderPeer{}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return prepared, factory
}

type noopSenderTerminal struct{}

func (noopSenderTerminal) StopRecovery()                 {}
func (noopSenderTerminal) Cleanup(context.Context) error { return nil }
func (noopSenderTerminal) Stop(context.Context) error    { return nil }

type noopSenderPeer struct{}

func (noopSenderPeer) HandleMessage(context.Context, protocolsession.Operation, protocolsession.Message) error {
	return nil
}
func (noopSenderPeer) Cancel(context.Context, protocolsession.Operation) error { return nil }
func (noopSenderPeer) Run(ctx context.Context) error                           { <-ctx.Done(); return ctx.Err() }

type testFramePipe struct {
	mu     sync.Mutex
	closed bool
	done   chan struct{}
	inbox  [2]chan framechannel.Frame
}

type testFrameEndpoint struct {
	pipe  *testFramePipe
	index int
}

func newTestFramePipe() (*testFrameEndpoint, *testFrameEndpoint) {
	pipe := &testFramePipe{
		done:  make(chan struct{}),
		inbox: [2]chan framechannel.Frame{make(chan framechannel.Frame, 256), make(chan framechannel.Frame, 256)},
	}
	return &testFrameEndpoint{pipe: pipe, index: 0}, &testFrameEndpoint{pipe: pipe, index: 1}
}

func (ep *testFrameEndpoint) Send(ctx context.Context, frame framechannel.Frame) error {
	ep.pipe.mu.Lock()
	defer ep.pipe.mu.Unlock()
	if ep.pipe.closed {
		return framechannel.RetireSend(io.ErrClosedPipe)
	}
	select {
	case <-ctx.Done():
		return framechannel.RejectSend(ctx.Err())
	case ep.pipe.inbox[1-ep.index] <- bytes.Clone(frame):
		return nil
	}
}

func (ep *testFrameEndpoint) SendTerminal(ctx context.Context, frame framechannel.Frame) error {
	return ep.Send(ctx, frame)
}

func (ep *testFrameEndpoint) Recv() <-chan framechannel.Frame { return ep.pipe.inbox[ep.index] }

func (ep *testFrameEndpoint) State() framechannel.ChannelState {
	ep.pipe.mu.Lock()
	defer ep.pipe.mu.Unlock()
	if ep.pipe.closed {
		return framechannel.Closed
	}
	return framechannel.Open
}

func (ep *testFrameEndpoint) Close() error {
	ep.pipe.mu.Lock()
	defer ep.pipe.mu.Unlock()
	if !ep.pipe.closed {
		ep.pipe.closed = true
		close(ep.pipe.done)
		close(ep.pipe.inbox[0])
		close(ep.pipe.inbox[1])
	}
	return nil
}

type dropAfterSendChannel struct {
	framechannel.Channel
	closePeer       func() error
	dropAfterFrames int32
	sent            atomic.Int32
}

func (ch *dropAfterSendChannel) Send(ctx context.Context, frame framechannel.Frame) error {
	if ch.sent.Add(1) > ch.dropAfterFrames {
		_ = ch.closePeer()
		return framechannel.RetireSend(io.ErrClosedPipe)
	}
	return ch.Channel.Send(ctx, frame)
}

type inMemoryReceiveConnection struct {
	descriptor []byte
	channel    *testFrameEndpoint
}

func (c *inMemoryReceiveConnection) Descriptor() []byte                    { return c.descriptor }
func (c *inMemoryReceiveConnection) Channel() protocolsession.FrameChannel { return c.channel }
func (c *inMemoryReceiveConnection) Done() <-chan struct{}                 { return c.channel.pipe.done }
func (c *inMemoryReceiveConnection) Err() error                            { return nil }
func (c *inMemoryReceiveConnection) Close() error                          { return c.channel.Close() }
