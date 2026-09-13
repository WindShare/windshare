package cli

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/internal/testoutputroot"
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
				receiverRecoveryOptions: relayset.ReceiverRecoveryOptions{Clock: clock, Jitter: func(delay time.Duration) time.Duration { return delay }},
				receiverDial: func(context.Context, relayv2.ReceiverConfig) (*relayv2.ReceiverConnection, error) {
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

func TestGetReceiverRecoveryOptionsFailure(t *testing.T) {
	app := &App{receiverRecoveryOptions: relayset.ReceiverRecoveryOptions{InitialWait: -1}}
	if _, code := app.connectGetReceiver(context.Background(), getRequest{}, getObservation{}); code != ExitFailure {
		t.Fatal(code)
	}
	recovery, _ := relayset.NewReceiverRecovery(relayset.ReceiverRecoveryOptions{})
	owner := &getReceiverRecovery{owner: recovery}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := owner.replace(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
