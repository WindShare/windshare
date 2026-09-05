package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/internal/testoutputroot"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

type disconnectingGetTrace struct {
	*recordingUserTrace
	once        sync.Once
	connections <-chan *relayv2.ReceiverConnection
}

func (trace *disconnectingGetTrace) Record(event clievent.Event) bool {
	if lifecycle, ok := event.(clievent.TransferLifecycleObserved); ok && lifecycle.Stage() == clievent.TransferFileFirstWrite {
		trace.once.Do(func() { connection := <-trace.connections; _ = connection.Channel().Close() })
	}
	return trace.recordingUserTrace.Record(event)
}

func TestGetReplacesLostSessionAndKeepsOneJobAndOutputReservation(t *testing.T) {
	server := newCLIRelayServer(t, &memoryStopStore{})
	stoppedEndpoint := newCLIRelayServer(t, &memoryStopStore{})
	filename := filepath.Join(t.TempDir(), "file.bin")
	payload := bytes.Repeat([]byte("preserved output"), 4096)
	if err := os.WriteFile(filename, payload, 0600); err != nil {
		t.Fatal(err)
	}
	shareOutput, shareErrors := &lockedTestBuffer{}, &lockedTestBuffer{}
	sender := &App{Stdout: shareOutput, Stderr: shareErrors, Stdin: strings.NewReader(""), revisionCapacity: newTestRevisionCapacity(t)}
	shareCtx, stopShare := context.WithCancel(context.Background())
	shared := make(chan int, 1)
	go func() {
		shared <- sender.Run(shareCtx, []string{"share", filename, "--relay", server.URL, "--relay", stoppedEndpoint.URL, "--block-size", strconv.Itoa(catalog.MinChunkSize)})
	}()
	defer func() {
		stopShare()
		select {
		case code := <-shared:
			if code != ExitOK {
				t.Errorf("share exit=%d stderr=%s", code, shareErrors.String())
			}
		case <-time.After(5 * time.Second):
			t.Error("share did not join")
		}
	}()
	capability := strings.TrimPrefix(waitTestLine(t, shareOutput, "Link: "), "Link: ")
	connections := make(chan *relayv2.ReceiverConnection, 8)
	trace := &disconnectingGetTrace{recordingUserTrace: newRecordingUserTrace(), connections: connections}
	var dials, primaryAttempts, stoppedAttempts atomic.Int32
	output := testoutputroot.New(t)
	getErrors := &lockedTestBuffer{}
	receiver := &App{Stdout: &lockedTestBuffer{}, Stderr: getErrors, Stdin: strings.NewReader(""),
		receiverDial: func(ctx context.Context, config relayv2.ReceiverConfig) (*relayv2.ReceiverConnection, error) {
			if config.RelayBaseURL == stoppedEndpoint.URL {
				stoppedAttempts.Add(1)
				return nil, &relayv2.RelayError{Code: v2.ErrorStopped}
			}
			if primaryAttempts.Add(1) == 2 {
				return nil, errors.New("transient endpoint failure during fresh join")
			}
			connection, err := relayv2.DialReceiver(ctx, config)
			if err == nil {
				dials.Add(1)
				connections <- connection
			}
			return connection, err
		},
		openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			return trace, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	code := receiver.Run(ctx, []string{"get", capability, "-o", output.RootPath, "--connectivity", "relay-only", "--trace", filepath.Join(t.TempDir(), "receive.ndjson")})
	if code != ExitOK {
		t.Fatalf("get=%d dials=%d stderr=%q", code, dials.Load(), getErrors.String())
	}
	if dials.Load() != 2 || primaryAttempts.Load() != 3 || stoppedAttempts.Load() != 2 {
		t.Fatalf("session generations=%d primary attempts=%d stopped attempts=%d", dials.Load(), primaryAttempts.Load(), stoppedAttempts.Load())
	}
	actual, err := os.ReadFile(filepath.Join(output.RootPath, "file.bin"))
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("published bytes=%d err=%v", len(actual), err)
	}
	trace.mu.Lock()
	events := append([]clievent.Event(nil), trace.events...)
	trace.mu.Unlock()
	jobs := map[clievent.TransferJobID]bool{}
	sessions := map[clievent.ProtocolSessionID]bool{}
	for _, event := range events {
		if lifecycle, ok := event.(clievent.TransferLifecycleObserved); ok {
			jobs[lifecycle.TransferJobID()] = true
			sessions[lifecycle.ProtocolSessionID()] = true
		}
	}
	if len(jobs) != 1 || len(sessions) != 2 {
		t.Fatalf("job identities=%d session generations=%d", len(jobs), len(sessions))
	}
}
