package main

import "testing"

func TestSessionDependenciesFollowOwnership(t *testing.T) {
	const session = coreImportPath + "/session"
	for _, test := range []struct {
		from, to  string
		forbidden bool
	}{
		{session + "/sendersession", session + "/receiversession", true},
		{session + "/receiversession", session + "/sendersession", true},
		{session + "/internal/sessioncore", session + "/receiversession", true},
		{session + "/internal/sessioncore", coreImportPath + "/transfer", true},
		{session + "/internal/sessioncore", "crypto/ed25519", true},
		{session + "/sendersession", "crypto/ed25519", false},
		{session + "/observation", session + "/internal/sessioncore", true},
		{session, session + "/sendersession", true},
		{session + "/sendersession", session + "/internal/sessioncore", false},
		{session + "/receiversession", coreImportPath + "/transfer", false},
		{session + "/internal/sessioncore", session + "/protocolsession", false},
	} {
		t.Run(test.from+" to "+test.to, func(t *testing.T) {
			if found := sessionDependencyViolations(productionScope, test.from, []string{test.to}); (len(found) > 0) != test.forbidden {
				t.Fatalf("dependency verdict=%v, forbidden=%v", found, test.forbidden)
			}
			if found := sessionDependencyViolations(testDeltaScope, test.from, []string{test.to}); len(found) != 0 {
				t.Fatalf("role integration test rejected: %v", found)
			}
		})
	}
}
