package cli

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/internal/diaglog"
)

func TestSharePublicationFailureStopsAndJoinsService(t *testing.T) {
	for _, test := range []struct {
		name              string
		invalidCapability bool
		shortWrite        bool
		cleanupError      error
		exitCode          int
		failure           engine.FailureCode
	}{
		{name: "encoding", invalidCapability: true, exitCode: ExitUsage, failure: engine.FailureCapabilityInvalid},
		{name: "output", exitCode: ExitFailure, failure: engine.FailurePublication},
		{name: "partial output", shortWrite: true, exitCode: ExitFailure, failure: engine.FailurePublication},
		{name: "output and cleanup", cleanupError: errors.New("relay cleanup failed"), exitCode: ExitFailure, failure: engine.FailurePublication},
	} {
		t.Run(test.name, func(t *testing.T) {
			relays := &cliShareRelays{
				accepting: make(chan struct{}), stopped: make(chan struct{}), cleanupStarted: make(chan context.Context, 1),
				cleanupRelease: make(chan struct{}), cleanupError: test.cleanupError,
			}
			prefetchStarted, sourceClosed := make(chan struct{}), make(chan struct{})
			ctx, cancel := context.WithCancel(context.Background())
			trace := newFakeUserTrace(diaglog.RunStatus{})
			app := &App{
				Stdout: &startedShareFailureWriter{ctx: ctx, accepting: relays.accepting, prefetched: prefetchStarted, short: test.shortWrite},
				Stderr: io.Discard,
				engineConfig: engine.Config{Share: engine.ShareDependencies{
					Prepare: func(ctx context.Context, config liveshare.SenderConfig) (engine.PreparedShare, error) {
						prepared, err := liveshare.PrepareSender(ctx, config)
						if prepared == nil {
							return nil, err
						}
						return &publicationPreparedShare{PreparedSender: prepared, invalidCapability: test.invalidCapability,
							prefetchStarted: prefetchStarted, closed: sourceClosed}, err
					},
					Relays:      func(context.Context, []relayprofile.Registration) (engine.ShareRelays, error) { return relays, nil },
					PollNetwork: func(ctx context.Context, _ func()) { <-ctx.Done() },
				}},
				openUserTrace: func(diaglog.Target, string, diaglog.RunOptions) (runTraceRecorder, error) {
					return trace, nil
				},
			}
			result, finished := make(chan int, 1), make(chan struct{})
			release := sync.OnceFunc(func() { close(relays.cleanupRelease) })
			t.Cleanup(func() {
				cancel()
				release()
				select {
				case <-finished:
				case <-time.After(cliShareCancellationTimeout):
					t.Error("failed share did not settle during cleanup")
				}
			})
			root, tracePath := t.TempDir(), filepath.Join(t.TempDir(), "share.ndjson")
			go func() {
				defer close(finished)
				result <- app.Run(ctx, []string{"share", root, "--relay", relays.ReadyRelayURL(), "--front-url", "https://front.example", "--trace", tracePath})
			}()
			select {
			case cleanup := <-relays.cleanupStarted:
				if cleanup.Err() != nil {
					t.Fatalf("output failure canceled cleanup: %v", cleanup.Err())
				}
			case code := <-result:
				t.Fatalf("command returned %d without cleaning up its running share", code)
			case <-time.After(cliShareCancellationTimeout):
				t.Fatal("output failure did not stop the share")
			}
			select {
			case code := <-result:
				t.Fatalf("command returned %d before cleanup completed", code)
			case <-sourceClosed:
				t.Fatal("source released before relay cleanup completed")
			default:
			}
			release()
			select {
			case code := <-result:
				if code != test.exitCode {
					t.Fatalf("exit=%d want=%d", code, test.exitCode)
				}
			case <-time.After(cliShareCancellationTimeout):
				t.Fatal("command did not return its publication error")
			}
			select {
			case <-sourceClosed:
			default:
				t.Fatal("command returned with source ownership still live")
			}
			var commandFailures, settlements int
			for _, rec := range trace.recorded() {
				switch rec.Event {
				case "command_failed":
					commandFailures++
					var p struct {
						Failure struct {
							Code string `json:"code"`
						} `json:"failure"`
					}
					decodeRecordPayload(t, rec.Payload, &p)
					if p.Failure.Code != string(test.failure) {
						t.Fatalf("publication failure lost: %+v", p)
					}
				case "engine_task_observed":
					var p struct {
						Stage          string `json:"stage"`
						Outcome        string `json:"outcome"`
						CleanupFailure *struct {
							Code string `json:"code"`
						} `json:"cleanup_failure"`
					}
					decodeRecordPayload(t, rec.Payload, &p)
					if p.Stage != "task_settled" {
						continue
					}
					settlements++
					wantOutcome := "stopped"
					if test.cleanupError != nil {
						wantOutcome = "failed"
					}
					if p.Outcome != wantOutcome || (p.CleanupFailure != nil) != (test.cleanupError != nil) {
						t.Fatalf("service settlement was replaced by presentation failure: %+v", p)
					}
				}
			}
			if commandFailures != 1 || settlements != 1 {
				t.Fatalf("command failures=%d engine settlements=%d", commandFailures, settlements)
			}
		})
	}
}

type publicationPreparedShare struct {
	*liveshare.PreparedSender
	invalidCapability       bool
	prefetchStarted, closed chan struct{}
}

func (prepared *publicationPreparedShare) Capability() link.Link {
	if prepared.invalidCapability {
		return link.Link{}
	}
	return prepared.PreparedSender.Capability()
}

func (prepared *publicationPreparedShare) NewRuntimeFactory(config liveshare.RuntimeFactoryConfig) (engine.ShareSessionFactory, error) {
	return prepared.PreparedSender.NewRuntimeFactory(config)
}

func (prepared *publicationPreparedShare) StartRootPrefetch() {
	prepared.PreparedSender.StartRootPrefetch()
	close(prepared.prefetchStarted)
}

func (prepared *publicationPreparedShare) Close() error {
	defer close(prepared.closed)
	return prepared.PreparedSender.Close()
}

type startedShareFailureWriter struct {
	ctx                   context.Context
	accepting, prefetched <-chan struct{}
	short                 bool
}

func (writer *startedShareFailureWriter) Write(payload []byte) (int, error) {
	for _, started := range []<-chan struct{}{writer.accepting, writer.prefetched} {
		select {
		case <-started:
		case <-writer.ctx.Done():
			return 0, writer.ctx.Err()
		}
	}
	if writer.short {
		return len(payload) - 1, nil
	}
	return 0, errors.New("stdout failed after service startup")
}
