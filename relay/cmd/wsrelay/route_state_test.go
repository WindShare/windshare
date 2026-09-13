package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/relay/signaling/v2route"
)

func TestRelayRouteStateLogsResumeIdentityAndDecision(t *testing.T) {
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	state, err := openRelayRoutes(context.Background(), t.TempDir(), v2route.Config{
		MaxRoutes: 1, MaxSessions: 1, MaxSessionsPerShare: 1,
	}, logf)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close(logf)
	init := v2.RegisterInit{
		Mode: v2.RegistrationResume, PKHash: v2.PKHash{1}, ShareInstance: v2.ShareInstance{2},
	}
	digest := sha256.Sum256(append([]byte("windshare/v2 share-id\x00"), init.PKHash[:]...))
	copy(init.ShareID[:], digest[:v2.ShareIDBytes])
	token := v2.ResumeToken{3}
	init.ResumeTokenHash = sha256.Sum256(token[:])
	if _, err := state.registry.BeginResume(context.Background(), init, token); err != nil {
		t.Fatal(err)
	}
	if len(logged) != 1 {
		t.Fatalf("resume logs = %v", logged)
	}
	for _, field := range []string{
		"wsrelay: resume", fmt.Sprintf("share_id=%x", init.ShareID), fmt.Sprintf("share_instance=%x", init.ShareInstance),
		"phase=credential", "outcome=accepted", "expected_generation=0", "current_generation=0",
		"expected_owner_generation=0", "new_owner_generation=0", "retired_sessions=0", "error=<nil>",
	} {
		if !strings.Contains(logged[0], field) {
			t.Fatalf("resume log lacks %q: %s", field, logged[0])
		}
	}
}

func TestRelayRouteStateRejectsInvalidRegistryConfiguration(t *testing.T) {
	_, err := openRelayRoutes(context.Background(), t.TempDir(), v2route.Config{}, t.Logf)
	if !errors.Is(err, v2route.ErrConfig) {
		t.Fatalf("invalid route registry = %v", err)
	}
}
