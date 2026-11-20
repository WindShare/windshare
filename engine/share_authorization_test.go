package engine

import (
	"errors"
	"fmt"
	"testing"

	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/engine/internal/networkruntime"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

func TestShareAuthorizationPreservesAdmissionAuthority(t *testing.T) {
	selection := networkruntime.Selection{Plan: relayplan.Plan{
		Configured: []relayplan.Candidate{{URL: "https://own.example"}},
		Network:    []relayplan.Candidate{{URL: "https://official.example"}},
	}}
	rejection := func(url string, code v2.ErrorCode) error {
		return &relayset.RegistrationFailure{URL: url, Cause: &relayv2.RelayError{Code: code}}
	}
	authFailure := func(url string, state senderrelay.AuthorizationState) error {
		return &relayset.RegistrationFailure{
			URL:   url,
			Cause: &senderrelay.AuthorizationError{State: state, Cause: errors.New("account_blocked")},
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
		{"official local denial", authFailure("https://official.example", senderrelay.AuthorizationDenied), ShareOfficialAuthorization, ""},
		{"explicit recovery first", errors.Join(
			rejection("https://official.example", v2.ErrorAccessDenied),
			fmt.Errorf("startup: %w", rejection("https://own.example", v2.ErrorAccessDenied)),
		), ShareSelfHostedAuthorization, "https://own.example"},
		{"explicit recovery before local official denial", errors.Join(
			authFailure("https://official.example", senderrelay.AuthorizationDenied),
			rejection("https://own.example", v2.ErrorAccessDenied),
		), ShareSelfHostedAuthorization, "https://own.example"},
		{"temporary", rejection("https://own.example", v2.ErrorAccessUnavailable), "", ""},
		{"temporary local authorization", authFailure("https://official.example", senderrelay.AuthorizationTemporaryFailure), "", ""},
		{"unbound rejection", &relayv2.RelayError{Code: v2.ErrorAccessDenied}, "", ""},
		{"unbound local denial", &senderrelay.AuthorizationError{State: senderrelay.AuthorizationDenied}, "", ""},
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
