package serviceauthority

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestPasswordVerifierContractVectors(t *testing.T) {
	data, err := os.ReadFile("../../testdata/network/password-verifiers.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Iterations    int               `json:"iterations"`
		VerifierBytes int               `json:"verifier_bytes"`
		Domains       map[string]string `json:"domains"`
		Vectors       []struct {
			Name              string `json:"name"`
			Role              string `json:"role"`
			Username          string `json:"username"`
			CanonicalUsername string `json:"canonical_username"`
			Password          string `json:"password"`
			Verifier          string `json:"verifier"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Iterations != PasswordPBKDF2Iterations || fixture.VerifierBytes != PasswordVerifierBytes {
		t.Fatalf("unexpected KDF parameters: %+v", fixture)
	}
	if fixture.Domains["account"] != AccountPasswordDomain || fixture.Domains["admin"] != AdminPasswordDomain {
		t.Fatalf("unexpected domains: %+v", fixture.Domains)
	}
	byName := make(map[string]string, len(fixture.Vectors))
	for _, vector := range fixture.Vectors {
		canonical, err := NormalizeUsername(vector.Username)
		if err != nil || canonical != vector.CanonicalUsername {
			t.Fatalf("%s: NormalizeUsername(%q) = (%q, %v), want %q", vector.Name, vector.Username, canonical, err, vector.CanonicalUsername)
		}
		got, err := DerivePasswordVerifier(PasswordRole(vector.Role), vector.Username, vector.Password)
		if err != nil {
			t.Fatalf("%s: DerivePasswordVerifier failed: %v", vector.Name, err)
		}
		if got != vector.Verifier {
			t.Fatalf("%s: verifier = %q, want %q", vector.Name, got, vector.Verifier)
		}
		byName[vector.Name] = got
	}
	if byName["account_mixed_case_whitespace"] == byName["admin_domain_separation"] {
		t.Fatal("account and admin password verifiers must stay domain-separated")
	}
}

func TestPasswordAndUsernameValidation(t *testing.T) {
	for _, invalid := range []string{"", "   ", "has space", "user@example", "名字", strings.Repeat("a", UsernameMaxBytes+1)} {
		if _, err := NormalizeUsername(invalid); !errors.Is(err, ErrFormat) {
			t.Errorf("NormalizeUsername(%q) = %v, want ErrFormat", invalid, err)
		}
	}
	if ValidNewPassword("too-short-1") || ValidNewPassword(strings.Repeat("a", PasswordMaxBytes+1)) {
		t.Fatal("ValidNewPassword accepted out-of-range length")
	}
	if !ValidNewPassword("12-byte-pass") || !ValidNewPassword(strings.Repeat("a", PasswordMaxBytes)) {
		t.Fatal("ValidNewPassword rejected boundary lengths")
	}
	if ValidLoginPassword("") || ValidLoginPassword(strings.Repeat("a", PasswordMaxBytes+1)) {
		t.Fatal("ValidLoginPassword accepted empty or oversized password")
	}
	if !ValidLoginPassword("x") || !ValidLoginPassword(strings.Repeat("a", PasswordMaxBytes)) {
		t.Fatal("ValidLoginPassword rejected boundary lengths")
	}
	if _, err := DerivePasswordVerifier(PasswordRole("other"), "alice", "valid-password"); !errors.Is(err, ErrFormat) {
		t.Fatalf("unknown role = %v, want ErrFormat", err)
	}
	if _, err := DerivePasswordVerifier(PasswordRoleAccount, "alice", ""); !errors.Is(err, ErrFormat) {
		t.Fatalf("empty password = %v, want ErrFormat", err)
	}
}
