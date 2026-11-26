package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/engine/internal/networkruntime"
	"github.com/windshare/windshare/engine/networkconfig"
	"github.com/windshare/windshare/engine/networkdirectory"
	"github.com/windshare/windshare/engine/relayprofile"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

func TestShareAuthorizationPreservesAdmissionAuthority(t *testing.T) {
	selection := networkruntime.Selection{Plan: relayplan.Plan{
		Configured: []relayplan.Candidate{{URL: "https://own.example"}},
		Network:    []relayplan.Candidate{{URL: "https://official.example"}},
		Standby:    []relayplan.Candidate{{URL: "https://canary.example", Canary: true}},
	}}
	rejection := func(url string, code v2.ErrorCode) error {
		return &relayset.RegistrationFailure{URL: url, Cause: &relayv2.RelayError{Code: code}}
	}
	authFailure := func(url string, state relayset.AuthorizationState) error {
		return &relayset.RegistrationFailure{
			URL:   url,
			Cause: &relayset.AuthorizationError{State: state, Cause: errors.New("account_blocked")},
		}
	}
	for _, test := range []struct {
		name string
		err  error
		kind ShareAuthorizationKind
		url  string
	}{
		{"self hosted", rejection("https://own.example", v2.ErrorAccessDenied), ShareSelfHostedAuthorization, "https://own.example"},
		{"official", rejection("https://official.example", v2.ErrorAccessDenied), ShareOfficialAuthorization, ""},
		{"official local denial", authFailure("https://official.example", relayset.AuthorizationDenied), ShareOfficialAuthorization, ""},
		{"official standby", rejection("https://canary.example", v2.ErrorAccessDenied), ShareOfficialAuthorization, ""},
		{"official standby local denial", authFailure("https://canary.example", relayset.AuthorizationDenied), ShareOfficialAuthorization, ""},
		{"official standby after non-auth active failure", errors.Join(
			rejection("https://official.example", v2.ErrorAccessUnavailable),
			rejection("https://canary.example", v2.ErrorAccessDenied),
		), ShareOfficialAuthorization, ""},
		{"explicit recovery first", errors.Join(
			rejection("https://official.example", v2.ErrorAccessDenied),
			fmt.Errorf("startup: %w", rejection("https://own.example", v2.ErrorAccessDenied)),
		), ShareSelfHostedAuthorization, "https://own.example"},
		{"explicit recovery before local official denial", errors.Join(
			authFailure("https://official.example", relayset.AuthorizationDenied),
			rejection("https://own.example", v2.ErrorAccessDenied),
		), ShareSelfHostedAuthorization, "https://own.example"},
		{"explicit recovery before standby denial", errors.Join(
			rejection("https://canary.example", v2.ErrorAccessDenied),
			rejection("https://own.example", v2.ErrorAccessDenied),
		), ShareSelfHostedAuthorization, "https://own.example"},
		{"temporary", rejection("https://own.example", v2.ErrorAccessUnavailable), "", ""},
		{"temporary standby", rejection("https://canary.example", v2.ErrorAccessUnavailable), "", ""},
		{"temporary local authorization", authFailure("https://official.example", relayset.AuthorizationTemporaryFailure), "", ""},
		{"unbound rejection", &relayv2.RelayError{Code: v2.ErrorAccessDenied}, "", ""},
		{"unbound local denial", &relayset.AuthorizationError{State: relayset.AuthorizationDenied}, "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := shareAuthorization(test.err, selection)
			if test.kind == "" {
				if got != nil {
					t.Fatal("unproven authorization requirement", got)
				}
			} else if got == nil || got.Kind != test.kind || got.RelayURL != test.url {
				t.Fatal("wrong authorization owner", got)
			}
		})
	}
}

func TestShareSnapshotTerminalAttributesOfficialAuthorizationAfterStandbyFallback(t *testing.T) {
	activeURLs := []string{"https://active1.example", "https://active2.example"}
	const canaryURL = "https://canary.example"
	now := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	roots, envelope := signedCanaryDirectory(t, now, activeURLs, canaryURL)

	startupErr := errors.Join(
		&relayset.RegistrationFailure{URL: activeURLs[0], Cause: &relayv2.RelayError{Code: v2.ErrorAccessUnavailable}},
		&relayset.RegistrationFailure{URL: activeURLs[1], Cause: &relayv2.RelayError{Code: v2.ErrorAccessUnavailable}},
		&relayset.RegistrationFailure{URL: canaryURL, Cause: &relayv2.RelayError{Code: v2.ErrorAccessDenied}},
	)
	relays := newFailingStatusRelays(startupErr)
	app := newTestEngine(t, Config{
		Clock:   doubleSpeedClock{origin: now, started: time.Now()},
		Network: &networkconfig.Config{Directory: networkdirectory.Config{Network: "test", Roots: roots, Embedded: envelope}},
		Share: ShareDependencies{
			Prepare: func(context.Context, liveshare.SenderConfig) (PreparedShare, error) {
				return &preparedFacadeShare{}, nil
			},
			Relays:      func(context.Context, []ShareRelayCandidate) (ShareRelays, error) { return relays, nil },
			PollNetwork: func(ctx context.Context, _ func()) { <-ctx.Done() },
		},
	})

	current, err := app.StartShare(t.Context(), ShareRequest{RelayIntent: relayprofile.Plan(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if len(current.selection.Plan.Network) != len(activeURLs) || len(current.selection.Plan.Standby) != 1 {
		t.Fatalf("unexpected plan: network=%v standby=%v", current.selection.Plan.Network, current.selection.Plan.Standby)
	}

	relays.fail()
	if _, err := current.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	terminal := current.Snapshot()
	if terminal.Authorization == nil || terminal.Authorization.Kind != ShareOfficialAuthorization || terminal.Authorization.RelayURL != "" {
		t.Fatalf("terminal authorization after standby denial = %+v, want official", terminal.Authorization)
	}
}
