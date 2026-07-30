package browsermatrixbroker

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type staticJWKSFetcher struct {
	document []byte
	calls    atomic.Int64
}

func (fetcher *staticJWKSFetcher) Fetch(context.Context, string) ([]byte, error) {
	fetcher.calls.Add(1)
	return append([]byte(nil), fetcher.document...), nil
}

type oidcClaims struct {
	Issuer      string `json:"iss"`
	Audience    string `json:"aud"`
	Repository  string `json:"repository"`
	Ref         string `json:"ref"`
	WorkflowRef string `json:"workflow_ref"`
	IssuedAt    int64  `json:"iat"`
	NotBefore   int64  `json:"nbf"`
	ExpiresAt   int64  `json:"exp"`
	JWTID       string `json:"jti"`
}

func TestOIDCValidatorExactClaimsAndConcurrentReplay(t *testing.T) {
	now := time.Date(2031, 3, 4, 5, 6, 7, 0, time.UTC)
	key, fetcher := newOIDCTestAuthority(t)
	policy := oidcTestPolicy(now, fetcher)
	validator, err := NewTestOIDCValidatorForHarness(policy)
	if err != nil {
		t.Fatal(err)
	}
	claims := validOIDCClaims(now, "jwt-id-00000001")
	assertion := signOIDCAssertion(t, key, claims)
	defer erase(assertion)
	if err := validator.Validate(context.Background(), assertion); err != nil {
		t.Fatal(err)
	}
	if err := validator.Validate(context.Background(), assertion); err == nil {
		t.Fatal("OIDC jti replay was accepted")
	}

	concurrent, err := NewTestOIDCValidatorForHarness(oidcTestPolicy(now, fetcher))
	if err != nil {
		t.Fatal(err)
	}
	assertion = signOIDCAssertion(t, key, validOIDCClaims(now, "jwt-id-concurrent"))
	defer erase(assertion)
	var successes atomic.Int64
	var wait sync.WaitGroup
	for range 16 {
		wait.Go(func() {
			if concurrent.Validate(context.Background(), assertion) == nil {
				successes.Add(1)
			}
		})
	}
	wait.Wait()
	if successes.Load() != 1 {
		t.Fatalf("concurrent replay successes=%d", successes.Load())
	}
}

func TestOIDCValidatorRejectsEveryClaimDrift(t *testing.T) {
	now := time.Date(2031, 3, 4, 5, 6, 7, 0, time.UTC)
	key, fetcher := newOIDCTestAuthority(t)
	mutations := []func(*oidcClaims){
		func(claims *oidcClaims) { claims.Issuer = "https://hostile.example" },
		func(claims *oidcClaims) { claims.Audience = "hostile-audience" },
		func(claims *oidcClaims) { claims.Repository = "hostile/repository" },
		func(claims *oidcClaims) { claims.Ref = "refs/heads/hostile" },
		func(claims *oidcClaims) {
			claims.WorkflowRef = "windshare/windshare/.github/workflows/hostile.yml@refs/heads/main"
		},
		func(claims *oidcClaims) { claims.NotBefore = now.Add(time.Minute).Unix() },
		func(claims *oidcClaims) { claims.ExpiresAt = now.Add(16 * time.Minute).Unix() },
	}
	for index, mutate := range mutations {
		validator, err := NewTestOIDCValidatorForHarness(oidcTestPolicy(now, fetcher))
		if err != nil {
			t.Fatal(err)
		}
		claims := validOIDCClaims(now, "jwt-id-drift-00000000")
		claims.JWTID = claims.JWTID[:len(claims.JWTID)-1] + string(rune('a'+index))
		mutate(&claims)
		assertion := signOIDCAssertion(t, key, claims)
		if err := validator.Validate(context.Background(), assertion); err == nil {
			erase(assertion)
			t.Fatalf("claim drift %d was accepted", index)
		}
		erase(assertion)
	}
}

func TestProductionOIDCConstructorPinsGitHubAndConcreteFetcher(t *testing.T) {
	now := time.Date(2031, 3, 4, 5, 6, 7, 0, time.UTC)
	_, fetcher := newOIDCTestAuthority(t)
	policy := oidcTestPolicy(now, fetcher)
	if _, err := NewOIDCValidator(policy); err == nil {
		t.Fatal("production OIDC admitted an injected JWKS fetcher")
	}
	policy.JWKSFetcher = nil
	policy.Issuer = "https://hostile.example"
	if _, err := NewOIDCValidator(policy); err == nil {
		t.Fatal("production OIDC admitted an alternate issuer")
	}
	policy.Issuer = GitHubActionsOIDCIssuer
	policy.JWKSURL = "https://hostile.example/jwks"
	if _, err := NewOIDCValidator(policy); err == nil {
		t.Fatal("production OIDC admitted an alternate JWKS endpoint")
	}
}

func newOIDCTestAuthority(t *testing.T) (*rsa.PrivateKey, *staticJWKSFetcher) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, minimumRSAKeyBits)
	if err != nil {
		t.Fatal(err)
	}
	document, err := json.Marshal(struct {
		Keys []map[string]string `json:"keys"`
	}{Keys: []map[string]string{{
		"kty": "RSA", "kid": "test-key", "use": "sig", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return key, &staticJWKSFetcher{document: document}
}

func oidcTestPolicy(now time.Time, fetcher JWKSFetcher) OIDCPolicy {
	return OIDCPolicy{
		Issuer: GitHubActionsOIDCIssuer, Audience: "windshare-browser-matrix",
		Repository: "windshare/windshare", Ref: "refs/heads/main",
		WorkflowRef: "windshare/windshare/.github/workflows/network.yml@refs/heads/main",
		JWKSURL:     GitHubActionsJWKSURL, MaximumTokenReplays: 64,
		Clock: brokerTestClock{now: now}, JWKSFetcher: fetcher,
	}
}

func validOIDCClaims(now time.Time, jwtID string) oidcClaims {
	return oidcClaims{
		Issuer: GitHubActionsOIDCIssuer, Audience: "windshare-browser-matrix",
		Repository: "windshare/windshare", Ref: "refs/heads/main",
		WorkflowRef: "windshare/windshare/.github/workflows/network.yml@refs/heads/main",
		IssuedAt:    now.Add(-time.Second).Unix(), NotBefore: now.Add(-time.Second).Unix(),
		ExpiresAt: now.Add(5 * time.Minute).Unix(), JWTID: jwtID,
	}
}

func signOIDCAssertion(t *testing.T, key *rsa.PrivateKey, claims oidcClaims) []byte {
	t.Helper()
	header, err := json.Marshal(struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
		Type      string `json:"typ"`
	}{Algorithm: "RS256", KeyID: "test-key", Type: "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	claimDocument, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signingInput := []byte(base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(claimDocument))
	digest := sha256.Sum256(signingInput)
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	assertion := append(signingInput, '.')
	assertion = append(assertion, base64.RawURLEncoding.EncodeToString(signature)...)
	erase(header)
	erase(claimDocument)
	erase(signingInput)
	erase(signature)
	return assertion
}
