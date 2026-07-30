package browsermatrixbroker

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	GitHubActionsOIDCIssuer = "https://token.actions.githubusercontent.com"
	GitHubActionsJWKSURL    = "https://token.actions.githubusercontent.com/.well-known/jwks"

	minimumRSAKeyBits       = 2048
	maximumRSAKeyBits       = 8192
	maximumJWKSBytes        = 1 << 20
	maximumJWTHeaderBytes   = 4096
	maximumJWTClaimsBytes   = 1 << 16
	maximumJWTLifetime      = 15 * time.Minute
	maximumAllowedClockSkew = 30 * time.Second
)

type Clock interface {
	Now() time.Time
}

type JWKSFetcher interface {
	Fetch(context.Context, string) ([]byte, error)
}

type WorkloadIdentityValidator interface {
	Validate(context.Context, []byte) error
}

type OIDCPolicy struct {
	Issuer              string
	Audience            string
	Repository          string
	Ref                 string
	WorkflowRef         string
	JWKSURL             string
	MaximumTokenReplays int
	Clock               Clock
	JWKSFetcher         JWKSFetcher
}

type OIDCValidator struct {
	policy OIDCPolicy
	clock  Clock

	mu             sync.Mutex
	acceptedJWTIDs map[string]time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func NewOIDCValidator(policy OIDCPolicy) (*OIDCValidator, error) {
	if policy.Issuer != GitHubActionsOIDCIssuer || policy.JWKSURL != GitHubActionsJWKSURL ||
		policy.JWKSFetcher != nil {
		return nil, errors.New("credential broker production OIDC authority is not GitHub Actions")
	}
	policy.JWKSFetcher = NewHTTPJWKSFetcher(nil)
	return newOIDCValidator(policy)
}

// NewTestOIDCValidatorForHarness is the only constructor that admits a
// non-production issuer or JWKS endpoint. Its name keeps that authority escape
// explicit at every call site.
func NewTestOIDCValidatorForHarness(policy OIDCPolicy) (*OIDCValidator, error) {
	return newOIDCValidator(policy)
}

func newOIDCValidator(policy OIDCPolicy) (*OIDCValidator, error) {
	clock := policy.Clock
	if clock == nil {
		clock = realClock{}
	}
	if !canonicalHTTPSOrigin(policy.Issuer) || !canonicalHTTPSURL(policy.JWKSURL) ||
		len(policy.Audience) < 8 || !validRepository(policy.Repository) ||
		!validGitRef(policy.Ref) || !validWorkflowRef(policy.WorkflowRef) ||
		policy.JWKSFetcher == nil || policy.MaximumTokenReplays < 1 ||
		policy.MaximumTokenReplays > 65_535 {
		return nil, errors.New("credential broker OIDC authority is incomplete")
	}
	policy.Clock = nil
	return &OIDCValidator{
		policy: policy, clock: clock, acceptedJWTIDs: make(map[string]time.Time),
	}, nil
}

func (validator *OIDCValidator) Validate(ctx context.Context, assertion []byte) error {
	if len(assertion) == 0 || len(assertion) > MaximumFrameBytes || ctx.Err() != nil {
		return errors.New("credential broker workload identity is invalid")
	}
	firstDot := bytes.IndexByte(assertion, '.')
	lastDot := bytes.LastIndexByte(assertion, '.')
	if firstDot <= 0 || lastDot <= firstDot+1 || lastDot == len(assertion)-1 ||
		bytes.IndexByte(assertion[firstDot+1:lastDot], '.') >= 0 {
		return errors.New("credential broker workload identity is invalid")
	}
	headerDocument, err := decodeJWTSection(assertion[:firstDot], maximumJWTHeaderBytes)
	if err != nil {
		return errors.New("credential broker workload identity is invalid")
	}
	defer erase(headerDocument)
	claimsDocument, err := decodeJWTSection(assertion[firstDot+1:lastDot], maximumJWTClaimsBytes)
	if err != nil {
		return errors.New("credential broker workload identity is invalid")
	}
	defer erase(claimsDocument)
	signature, err := decodeJWTSection(assertion[lastDot+1:], rsaSignatureMaximumBytes)
	if err != nil {
		return errors.New("credential broker workload identity is invalid")
	}
	defer erase(signature)

	headerFields, err := decodeUniqueObject(headerDocument)
	defer eraseRawFields(headerFields)
	if err != nil || len(headerFields) != 3 || stringField(headerFields, "alg") != "RS256" ||
		stringField(headerFields, "typ") != "JWT" {
		return errors.New("credential broker workload identity header is invalid")
	}
	keyID := stringField(headerFields, "kid")
	if !validKeyID(keyID) {
		return errors.New("credential broker workload identity header is invalid")
	}
	jwksDocument, err := validator.policy.JWKSFetcher.Fetch(ctx, validator.policy.JWKSURL)
	if err != nil || len(jwksDocument) == 0 || len(jwksDocument) > maximumJWKSBytes {
		erase(jwksDocument)
		return errors.New("credential broker JWKS authority is unavailable")
	}
	publicKey, err := parseRS256Key(jwksDocument, keyID)
	erase(jwksDocument)
	if err != nil {
		return errors.New("credential broker JWKS authority is invalid")
	}
	signedDigest := sha256.Sum256(assertion[:lastDot])
	if rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, signedDigest[:], signature) != nil {
		return errors.New("credential broker workload identity signature is invalid")
	}

	claims, err := decodeUniqueObject(claimsDocument)
	defer eraseRawFields(claims)
	if err != nil || stringField(claims, "iss") != validator.policy.Issuer ||
		stringField(claims, "aud") != validator.policy.Audience ||
		stringField(claims, "repository") != validator.policy.Repository ||
		stringField(claims, "ref") != validator.policy.Ref ||
		stringField(claims, "workflow_ref") != validator.policy.WorkflowRef {
		return errors.New("credential broker workload identity claims are invalid")
	}
	issuedAt, issuedErr := integerDate(claims, "iat")
	notBefore, notBeforeErr := integerDate(claims, "nbf")
	expiresAt, expiresErr := integerDate(claims, "exp")
	jwtID := stringField(claims, "jti")
	now := validator.clock.Now().UTC()
	if issuedErr != nil || notBeforeErr != nil || expiresErr != nil || !validJWTID(jwtID) ||
		issuedAt.After(now.Add(maximumAllowedClockSkew)) ||
		notBefore.After(now.Add(maximumAllowedClockSkew)) || !now.Before(expiresAt) ||
		!expiresAt.After(issuedAt) || expiresAt.Sub(issuedAt) > maximumJWTLifetime {
		return errors.New("credential broker workload identity lifetime is invalid")
	}

	validator.mu.Lock()
	defer validator.mu.Unlock()
	for acceptedID, expiry := range validator.acceptedJWTIDs {
		if !now.Before(expiry) {
			delete(validator.acceptedJWTIDs, acceptedID)
		}
	}
	if _, replayed := validator.acceptedJWTIDs[jwtID]; replayed {
		return errors.New("credential broker workload identity was replayed")
	}
	if len(validator.acceptedJWTIDs) >= validator.policy.MaximumTokenReplays {
		return errors.New("credential broker workload identity replay authority is exhausted")
	}
	validator.acceptedJWTIDs[jwtID] = expiresAt
	return nil
}

const rsaSignatureMaximumBytes = maximumRSAKeyBits / 8

func decodeJWTSection(encoded []byte, maximumDecodedBytes int) ([]byte, error) {
	if len(encoded) == 0 || bytes.IndexByte(encoded, '=') >= 0 {
		return nil, errors.New("JWT section is invalid")
	}
	decodedLength := base64.RawURLEncoding.DecodedLen(len(encoded))
	if decodedLength == 0 || decodedLength > maximumDecodedBytes {
		return nil, errors.New("JWT section is outside authority")
	}
	decoded := make([]byte, decodedLength)
	written, err := base64.RawURLEncoding.Decode(decoded, encoded)
	if err != nil {
		erase(decoded)
		return nil, err
	}
	decoded = decoded[:written]
	reencoded := make([]byte, base64.RawURLEncoding.EncodedLen(len(decoded)))
	base64.RawURLEncoding.Encode(reencoded, decoded)
	canonical := bytes.Equal(reencoded, encoded)
	erase(reencoded)
	if !canonical {
		erase(decoded)
		return nil, errors.New("JWT section is not canonical")
	}
	return decoded, nil
}

func decodeUniqueObject(document []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(document))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errors.New("JSON object is invalid")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		nameToken, tokenErr := decoder.Token()
		name, ok := nameToken.(string)
		if tokenErr != nil || !ok {
			return nil, errors.New("JSON object field is invalid")
		}
		if _, exists := fields[name]; exists {
			return nil, errors.New("JSON object field is duplicated")
		}
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil {
			return nil, errors.New("JSON object value is invalid")
		}
		fields[name] = raw
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return nil, errors.New("JSON object is incomplete")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("JSON object has trailing data")
	}
	return fields, nil
}

func stringField(fields map[string]json.RawMessage, name string) string {
	value, exists := fields[name]
	if !exists {
		return ""
	}
	var decoded string
	if json.Unmarshal(value, &decoded) != nil {
		return ""
	}
	return decoded
}

func integerDate(fields map[string]json.RawMessage, name string) (time.Time, error) {
	value, exists := fields[name]
	if !exists {
		return time.Time{}, errors.New("integer date is unavailable")
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if decoder.Decode(&decoded) != nil {
		return time.Time{}, errors.New("integer date is invalid")
	}
	number, ok := decoded.(json.Number)
	seconds, err := number.Int64()
	if !ok || err != nil || seconds <= 0 {
		return time.Time{}, errors.New("integer date is invalid")
	}
	return time.Unix(seconds, 0).UTC(), nil
}

func parseRS256Key(document []byte, keyID string) (*rsa.PublicKey, error) {
	fields, err := decodeUniqueObject(document)
	if err != nil || len(fields) != 1 {
		return nil, errors.New("JWKS document is invalid")
	}
	var keys []json.RawMessage
	if json.Unmarshal(fields["keys"], &keys) != nil || len(keys) == 0 || len(keys) > 128 {
		return nil, errors.New("JWKS keys are invalid")
	}
	seen := make(map[string]struct{}, len(keys))
	var matched *rsa.PublicKey
	for _, raw := range keys {
		jwk, objectErr := decodeUniqueObject(raw)
		if objectErr != nil {
			return nil, errors.New("JWK is invalid")
		}
		currentID := stringField(jwk, "kid")
		if !validKeyID(currentID) {
			return nil, errors.New("JWK key ID is invalid")
		}
		if _, duplicate := seen[currentID]; duplicate {
			return nil, errors.New("JWK key ID is duplicated")
		}
		seen[currentID] = struct{}{}
		if currentID != keyID {
			continue
		}
		if stringField(jwk, "kty") != "RSA" || stringField(jwk, "alg") != "RS256" ||
			stringField(jwk, "use") != "sig" {
			return nil, errors.New("JWK signing authority is invalid")
		}
		modulus, modulusErr := decodeJWKInteger(stringField(jwk, "n"), maximumRSAKeyBits/8)
		exponentBytes, exponentErr := decodeJWKInteger(stringField(jwk, "e"), 8)
		if modulusErr != nil || exponentErr != nil {
			return nil, errors.New("JWK RSA authority is invalid")
		}
		exponent := new(big.Int).SetBytes(exponentBytes)
		erase(exponentBytes)
		if !exponent.IsInt64() || exponent.Int64() < 3 || exponent.Int64() > 1<<31-1 || exponent.Bit(0) == 0 {
			erase(modulus)
			return nil, errors.New("JWK RSA exponent is invalid")
		}
		key := &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: int(exponent.Int64())}
		erase(modulus)
		if key.N.BitLen() < minimumRSAKeyBits || key.N.BitLen() > maximumRSAKeyBits {
			return nil, errors.New("JWK RSA modulus is outside authority")
		}
		matched = key
	}
	if matched == nil {
		return nil, errors.New("JWK key ID is unavailable")
	}
	return matched, nil
}

func decodeJWKInteger(value string, maximum int) ([]byte, error) {
	if value == "" || strings.Contains(value, "=") {
		return nil, errors.New("JWK integer is invalid")
	}
	return decodeJWTSection([]byte(value), maximum)
}

func validKeyID(value string) bool {
	return len(value) >= 1 && len(value) <= 256 && !strings.ContainsAny(value, "\x00\r\n")
}

func validJWTID(value string) bool {
	return len(value) >= 8 && len(value) <= 256 && !strings.ContainsAny(value, "\x00\r\n")
}

func validRepository(value string) bool {
	parts := strings.Split(value, "/")
	return len(parts) == 2 && validRepositoryPart(parts[0]) && validRepositoryPart(parts[1])
}

func validRepositoryPart(value string) bool {
	if value == "" {
		return false
	}
	for _, current := range value {
		if !(current >= 'A' && current <= 'Z' || current >= 'a' && current <= 'z' ||
			current >= '0' && current <= '9' || strings.ContainsRune("_.-", current)) {
			return false
		}
	}
	return true
}

func validGitRef(value string) bool {
	prefix := ""
	if strings.HasPrefix(value, "refs/heads/") {
		prefix = "refs/heads/"
	} else if strings.HasPrefix(value, "refs/tags/") {
		prefix = "refs/tags/"
	}
	return prefix != "" && len(value) > len(prefix) &&
		!strings.ContainsAny(value, "\x00\r\n @~^:?*[\\")
}

func validWorkflowRef(value string) bool {
	separator := strings.Index(value, "/.github/workflows/")
	at := strings.LastIndex(value, "@")
	return separator > 0 && at > separator && validRepository(value[:separator]) &&
		validGitRef(value[at+1:]) && !strings.ContainsAny(value[separator+1:at], "\x00\r\n @")
}

func canonicalHTTPSOrigin(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil &&
		parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.String() == value
}

func canonicalHTTPSURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil &&
		parsed.Path != "" && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.String() == value
}

func eraseRawFields(fields map[string]json.RawMessage) {
	for name, value := range fields {
		erase(value)
		delete(fields, name)
	}
}
