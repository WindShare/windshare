//go:build windows

package testnetwork

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChildAuthorizationBindingIsCanonicalAndOperationBound(t *testing.T) {
	binding, pipeName, err := newChildAuthorizationBinding("operation-a")
	if err != nil {
		t.Fatal(err)
	}
	parsed, delegated, err := parseChildAuthorizationPipeName(pipeName)
	if err != nil || !delegated || parsed != binding {
		t.Fatalf("binding round trip = %#v, %t, %v", parsed, delegated, err)
	}
	other, _, err := newChildAuthorizationBinding("operation-b")
	if err != nil {
		t.Fatal(err)
	}
	if binding.OperationSHA256 == other.OperationSHA256 {
		t.Fatal("distinct operations shared one child authority binding")
	}
	for _, invalid := range []string{
		childAuthorizationPipePrefix,
		childAuthorizationPipePrefix + strings.Repeat("A", 32) + "-" + strings.Repeat("b", 64),
		childAuthorizationPipePrefix + strings.Repeat("a", 31) + "-" + strings.Repeat("b", 64),
		childAuthorizationPipePrefix + strings.Repeat("g", 32) + "-" + strings.Repeat("b", 64),
	} {
		if _, delegated, err := parseChildAuthorizationPipeName(invalid); !delegated || err == nil {
			t.Fatalf("invalid child binding accepted: %q", invalid)
		}
	}
	if _, delegated, err := parseChildAuthorizationPipeName("windshare-d5-auth-root"); err != nil || delegated {
		t.Fatalf("root pipe classified as child: delegated=%t err=%v", delegated, err)
	}
}

func TestValidateParentAuthorizationRequiresExactChildBinding(t *testing.T) {
	binding := childAuthorizationBinding{
		InvocationID:    strings.Repeat("a", hex.EncodedLen(childAuthorizationInvocationBytes)),
		OperationSHA256: strings.Repeat("b", hex.EncodedLen(sha256.Size)),
	}
	payload := authorizationPayload{
		SchemaVersion: 1,
		RunID:         "delegated-run",
		Programs: []authorizedProgram{{
			Path: `C:\fixture\child.exe`, Bytes: 1, SHA256: strings.Repeat("c", hex.EncodedLen(sha256.Size)),
		}},
		AuthorityKind:   childAuthorizationKind,
		InvocationID:    binding.InvocationID,
		OperationSHA256: binding.OperationSHA256,
	}
	if _, err := validateParentAuthorization(payload, binding, true); err != nil {
		t.Fatalf("exact delegated binding rejected: %v", err)
	}
	wrongOperation := binding
	wrongOperation.OperationSHA256 = strings.Repeat("d", hex.EncodedLen(sha256.Size))
	if _, err := validateParentAuthorization(payload, wrongOperation, true); err == nil {
		t.Fatal("delegated payload crossed its operation binding")
	}
	if _, err := validateParentAuthorization(payload, childAuthorizationBinding{}, false); err == nil {
		t.Fatal("root authorization accepted delegated authority fields")
	}
}

func TestUnconsumedChildAuthorizationRetiresWithoutBlocking(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	program := authorizedProgram{
		Path:   executable,
		Bytes:  int64(len(data)),
		SHA256: hex.EncodeToString(digest[:]),
	}
	parent := processAuthorization{
		runID: "unconsumed-child-retirement",
		programs: map[string]authorizedProgram{
			strings.ToLower(filepath.Clean(executable)): program,
		},
	}
	_, retire, err := newOSNetworkChildAuthorityFor(parent, executable, "unconsumed-child-operation")
	if err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() { result <- retire() }()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("retire unconsumed child authority: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("unconsumed child authority retirement blocked")
	}
	if err := retire(); err != nil {
		t.Fatalf("repeat child authority retirement: %v", err)
	}
}
