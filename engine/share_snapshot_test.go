package engine

import (
	"bytes"
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/engine/internal/share"
	"github.com/windshare/windshare/engine/internal/task"
	"github.com/windshare/windshare/engine/relayprofile"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

const terminalSnapshotRaceIterations = 32

func TestShareSnapshotTerminalIncludesOmissionsRecordedBeforeDone(t *testing.T) {
	const relayURL = "https://relay.example"
	dirID, _ := catalog.DirectoryIDFromBytes([]byte{9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9})
	expected := catalog.SourceOmissions{
		Directory: dirID,
		Count:     2,
		Samples: []catalog.SourceOmission{
			{Name: "cloud.docx", Reason: catalog.SourceOmissionCloudPlaceholder},
		},
	}

	for range terminalSnapshotRaceIterations {
		var observer catalog.SourceOmissionObserver
		prepared := &closingOmissionPreparedShare{
			onClose: func() {
				if observer != nil {
					observer.ObserveSourceOmissions(expected)
				}
			},
		}
		application := newTestEngine(t, Config{Share: ShareDependencies{
			Peers: func() (SharePeers, error) { return &facadePeers{}, nil },
			Prepare: func(ctx context.Context, config liveshare.SenderConfig) (PreparedShare, error) {
				if _, err := config.Source.OpenFileSource(ctx, liveshare.FileSourceContext{}); err != nil {
					return nil, err
				}
				return prepared, nil
			},
			Relays:      func(context.Context, []ShareRelayCandidate) (ShareRelays, error) { return &facadeRelays{}, nil },
			PollNetwork: func(ctx context.Context, _ func()) { <-ctx.Done() },
		}})

		current, err := application.StartShare(t.Context(), ShareRequest{
			Source: liveshare.FileSourceFactoryFunc(func(_ context.Context, source liveshare.FileSourceContext) (liveshare.FileSource, error) {
				observer = source.Omissions
				return nil, nil
			}),
			RelayIntent: relayprofile.UseExplicit([]relayprofile.Candidate{{PublicURL: relayURL}}),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := current.Ready(t.Context()); err != nil {
			t.Fatal(err)
		}

		go current.StopShare()
		var terminal ShareSnapshot
		for {
			snapshot := current.Snapshot()
			if (snapshot.State == TaskFinished) != (snapshot.Result != nil) {
				t.Fatalf("torn snapshot: state=%v result=%+v", snapshot.State, snapshot.Result)
			}
			if snapshot.Result != nil {
				terminal = snapshot
				break
			}
			runtime.Gosched()
		}
		if terminal.State != TaskFinished {
			t.Fatalf("terminal state = %v, want %v", terminal.State, TaskFinished)
		}
		if terminal.SourceOmissions == nil || terminal.SourceOmissions.Count != expected.Count ||
			len(terminal.SourceOmissions.Samples) != 1 || terminal.SourceOmissions.Samples[0] != expected.Samples[0] {
			t.Fatalf("terminal snapshot tore SourceOmissions: %+v", terminal.SourceOmissions)
		}
	}
}

func TestShareSnapshotTerminalPreservesStartupFailureAndRelayStatus(t *testing.T) {
	const relayURL = "https://relay.example"
	deniedErr := &relayset.RegistrationFailure{
		URL:   relayURL,
		Cause: &relayv2.RelayError{Code: v2.ErrorAccessDenied},
	}

	for range terminalSnapshotRaceIterations {
		relays := newFailingStatusRelays(deniedErr)
		application := newTestEngine(t, Config{Share: ShareDependencies{
			Prepare: func(context.Context, liveshare.SenderConfig) (PreparedShare, error) {
				return &preparedFacadeShare{}, nil
			},
			Relays:      func(context.Context, []ShareRelayCandidate) (ShareRelays, error) { return relays, nil },
			PollNetwork: func(ctx context.Context, _ func()) { <-ctx.Done() },
		}})

		current, err := application.StartShare(t.Context(), ShareRequest{
			RelayIntent: relayprofile.UseExplicit([]relayprofile.Candidate{{PublicURL: relayURL}}),
		})
		if err != nil {
			t.Fatal(err)
		}

		go relays.fail()
		var terminal ShareSnapshot
		for {
			snapshot := current.Snapshot()
			if (snapshot.State == TaskFinished) != (snapshot.Result != nil) {
				t.Fatalf("torn snapshot: state=%v result=%+v", snapshot.State, snapshot.Result)
			}
			if snapshot.Result != nil {
				terminal = snapshot
				break
			}
			runtime.Gosched()
		}
		if terminal.State != TaskFinished || terminal.Result.Outcome != OutcomeFailed {
			t.Fatalf("unexpected terminal outcome: state=%v result=%+v", terminal.State, terminal.Result)
		}
		if terminal.Startup != ShareStartupFailed || terminal.StartupError == nil {
			t.Fatalf("terminal snapshot tore startup failure: startup=%v err=%v", terminal.Startup, terminal.StartupError)
		}
		if terminal.Authorization == nil || terminal.Authorization.Kind != ShareSelfHostedAuthorization ||
			terminal.Authorization.RelayURL != relayURL {
			t.Fatalf("terminal snapshot tore authorization: %+v", terminal.Authorization)
		}
		if terminal.RelayAvailability == nil || terminal.RelayAvailability.Terminal != 1 || terminal.RelayAvailability.Available != 0 {
			t.Fatalf("terminal snapshot tore relay availability: %+v", terminal.RelayAvailability)
		}
	}
}

type closingOmissionPreparedShare struct {
	preparedFacadeShare
	onClose func()
}

func (prepared *closingOmissionPreparedShare) Close() error {
	if prepared.onClose != nil {
		prepared.onClose()
	}
	return prepared.preparedFacadeShare.Close()
}

type failingStatusRelays struct {
	facadeRelays
	mu      sync.Mutex
	status  relayset.SenderStatus
	release chan struct{}
	once    sync.Once
	waitErr error
}

func newFailingStatusRelays(waitErr error) *failingStatusRelays {
	return &failingStatusRelays{
		status: relayset.SenderStatus{
			Availability: relayset.SenderAvailability{Available: 1, Total: 1},
		},
		release: make(chan struct{}),
		waitErr: waitErr,
	}
}

func (relays *failingStatusRelays) fail() {
	relays.once.Do(func() {
		relays.mu.Lock()
		relays.status = relayset.SenderStatus{
			Availability: relayset.SenderAvailability{Available: 0, Total: 1, Terminal: 1},
		}
		relays.mu.Unlock()
		close(relays.release)
	})
}

func (relays *failingStatusRelays) Status() relayset.SenderStatus {
	relays.mu.Lock()
	defer relays.mu.Unlock()
	return relays.status
}

func (*failingStatusRelays) ObserveStatus(func(relayset.SenderStatus)) {}

func (relays *failingStatusRelays) WaitReady(ctx context.Context) error {
	select {
	case <-relays.release:
		return relays.waitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestShareSnapshotNeverTearsFinishedWithoutResultDuringCompleted(t *testing.T) {
	for _, stopShare := range []bool{false, true} {
		name := "natural completion"
		wantIntermediate := TaskRunning
		if stopShare {
			name = "stopped completion"
			wantIntermediate = TaskStopping
		}
		t.Run(name, func(t *testing.T) {
			entered, releaseRun := make(chan struct{}), make(chan struct{})
			joining, releaseCompleted := make(chan struct{}), make(chan struct{})
			raw, err := task.Start(t.Context(), task.Config[ShareResult]{
				ID:                  "share-completed-window",
				Now:                 func() time.Time { return time.Unix(100, 0) },
				Random:              bytes.NewReader([]byte{1}),
				CleanupTimeout:      time.Minute,
				ObservationCapacity: 4,
				Run: func(context.Context, task.Control) task.Completion[ShareResult] {
					close(entered)
					<-releaseRun
					return task.Completion[ShareResult]{Outcome: OutcomeStopped}
				},
				Completed: func(error) {
					close(joining)
					<-releaseCompleted
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			current := &ShareTask{Task: &Task[ShareResult]{current: raw}, controller: share.NewController()}
			<-entered
			if stopShare {
				current.StopShare()
			}
			close(releaseRun)
			<-joining

			snapshot := current.Snapshot()
			if snapshot.State != wantIntermediate || snapshot.Result != nil {
				t.Fatalf("snapshot during Completed = (state=%v, result=%+v), want (state=%v, result=nil)", snapshot.State, snapshot.Result, wantIntermediate)
			}

			close(releaseCompleted)
			<-current.Done()
			terminal := current.Snapshot()
			if terminal.State != TaskFinished || terminal.Result == nil || terminal.Result.Outcome != OutcomeStopped {
				t.Fatalf("terminal snapshot = (state=%v, result=%+v), want Finished with OutcomeStopped", terminal.State, terminal.Result)
			}
		})
	}
}
