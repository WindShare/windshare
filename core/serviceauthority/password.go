package serviceauthority

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

const (
	UsernameMaxBytes         = 64
	PasswordMinBytes         = 12
	PasswordMaxBytes         = 256
	PasswordVerifierBytes    = 32
	PasswordPBKDF2Iterations = 600_000
	AccountPasswordDomain    = "windshare-account-password-v1:"
	AdminPasswordDomain      = "windshare-admin-password-v1:"
)

// PasswordRole separates client-derived password verifiers across distinct
// authority namespaces so identical credentials never collide between roles.
type PasswordRole string

const (
	PasswordRoleAccount PasswordRole = "account"
	PasswordRoleAdmin   PasswordRole = "admin"
)

func (role PasswordRole) domainPrefix() (string, bool) {
	switch role {
	case PasswordRoleAccount:
		return AccountPasswordDomain, true
	case PasswordRoleAdmin:
		return AdminPasswordDomain, true
	default:
		return "", false
	}
}

// NormalizeUsername trims surrounding whitespace, lowercases ASCII letters, and
// verifies the authority's canonical identifier alphabet.
func NormalizeUsername(raw string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(raw))
	if name == "" || len(name) > UsernameMaxBytes {
		return "", ErrFormat
	}
	for i := range len(name) {
		b := name[i]
		if (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '.' || b == '_' || b == '-' {
			continue
		}
		return "", ErrFormat
	}
	return name, nil
}

func ValidNewPassword(password string) bool {
	return len(password) >= PasswordMinBytes && len(password) <= PasswordMaxBytes
}

func ValidLoginPassword(password string) bool {
	return len(password) > 0 && len(password) <= PasswordMaxBytes
}

// DerivePasswordVerifier stretches a password locally with PBKDF2-HMAC-SHA256
// using a deterministic, role-separated username salt so plaintext passwords
// never cross the wire and offline brute-force work stays off the authority.
func DerivePasswordVerifier(role PasswordRole, username, password string) (string, error) {
	prefix, ok := role.domainPrefix()
	if !ok || !ValidLoginPassword(password) {
		return "", ErrFormat
	}
	canonical, err := NormalizeUsername(username)
	if err != nil {
		return "", err
	}
	derived, err := pbkdf2.Key(
		sha256.New,
		password,
		[]byte(prefix+canonical),
		PasswordPBKDF2Iterations,
		PasswordVerifierBytes,
	)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(derived), nil
}
