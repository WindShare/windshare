package browsermatrixturnprovider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/windshare/windshare/internal/browsermatrixbroker"
)

const (
	ProtocolVersion = "windshare.browser-network-matrix.turn-provider/v1"

	acquirePath = "/v1/leases/acquire"
	bindPath    = "/v1/leases/bind-and-wait"
	revokePath  = "/v1/leases/revoke-and-wait"

	maximumResponseBytes = 8 * 1024
	dialTimeout          = 10 * time.Second
	tlsHandshakeTimeout  = 10 * time.Second
)

type Config struct {
	Origin                  string
	TLSCertificateAuthority []byte
	ServerCertificateSHA256 string
	ClientCertificate       tls.Certificate
}

// Client is a mutually authenticated, certificate-pinned adapter for a coturn
// lease authority. The provider owns credential creation and revocation; the
// broker only transports a one-attempt capability into the Pion authority.
type Client struct {
	origin *url.URL
	http   httpDoer
}

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type acquireRequest struct {
	ProtocolVersion string `json:"protocolVersion"`
	Operation       string `json:"operation"`
	RequestID       string `json:"requestId"`
	RunID           string `json:"runId"`
	ProfileID       string `json:"profileId"`
	ProbeNonce      string `json:"probeNonce"`
	ExpiresAt       string `json:"expiresAt"`
	MaxAttempts     int    `json:"maxAttempts"`
}

type acquireResponse struct {
	ProtocolVersion string `json:"protocolVersion"`
	Operation       string `json:"operation"`
	ProviderLeaseID string `json:"providerLeaseId"`
	RequestID       string `json:"requestId"`
	RunID           string `json:"runId"`
	ProfileID       string `json:"profileId"`
	ProbeNonce      string `json:"probeNonce"`
	CredentialID    string `json:"credentialId"`
	Username        string `json:"username"`
	ExpiresAt       string `json:"expiresAt"`
	MaxAttempts     int    `json:"maxAttempts"`
	Credential      []byte `json:"credential"`
}

type bindRequest struct {
	ProtocolVersion   string `json:"protocolVersion"`
	Operation         string `json:"operation"`
	ProviderLeaseID   string `json:"providerLeaseId"`
	RequestID         string `json:"requestId"`
	RunID             string `json:"runId"`
	ProfileID         string `json:"profileId"`
	ProbeNonce        string `json:"probeNonce"`
	ControlLeaseID    string `json:"controlLeaseId"`
	AttestationSHA256 string `json:"attestationSha256"`
	CredentialID      string `json:"credentialId"`
	Username          string `json:"username"`
	ExpiresAt         string `json:"expiresAt"`
	MaxAttempts       int    `json:"maxAttempts"`
}

type bindResponse bindRequest

type revokeRequest struct {
	ProtocolVersion   string `json:"protocolVersion"`
	Operation         string `json:"operation"`
	RetirementReason  string `json:"retirementReason"`
	RequestID         string `json:"requestId"`
	ProviderLeaseID   string `json:"providerLeaseId"`
	ControlLeaseID    string `json:"controlLeaseId"`
	RunID             string `json:"runId"`
	ProfileID         string `json:"profileId"`
	ProbeNonce        string `json:"probeNonce"`
	AttestationSHA256 string `json:"attestationSha256"`
}

type revokeResponse struct {
	ProtocolVersion string `json:"protocolVersion"`
	Operation       string `json:"operation"`
	RequestID       string `json:"requestId"`
	ProviderLeaseID string `json:"providerLeaseId"`
	Terminal        string `json:"terminal"`
}

func New(config Config) (*Client, error) {
	origin, err := parseOrigin(config.Origin)
	if err != nil {
		return nil, err
	}
	expectedDigest, err := hex.DecodeString(config.ServerCertificateSHA256)
	if err != nil || len(expectedDigest) != sha256.Size {
		return nil, errors.New("TURN provider server certificate digest is invalid")
	}
	roots := x509.NewCertPool()
	if len(config.TLSCertificateAuthority) == 0 ||
		!roots.AppendCertsFromPEM(config.TLSCertificateAuthority) {
		return nil, errors.New("TURN provider certificate authority is invalid")
	}
	if len(config.ClientCertificate.Certificate) == 0 || config.ClientCertificate.PrivateKey == nil {
		return nil, errors.New("TURN provider client identity is invalid")
	}
	clientLeaf, err := x509.ParseCertificate(config.ClientCertificate.Certificate[0])
	if err != nil || !certificateAllowsClientAuthentication(clientLeaf, time.Now().UTC()) {
		return nil, errors.New("TURN provider client identity is invalid")
	}
	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		ServerName:   origin.Hostname(),
		RootCAs:      roots,
		Certificates: []tls.Certificate{config.ClientCertificate},
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("TURN provider server identity is unavailable")
			}
			observed := sha256.Sum256(state.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare(observed[:], expectedDigest) != 1 {
				return errors.New("TURN provider server identity is not pinned")
			}
			return nil
		},
	}
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:   false,
		DisableCompression:  true,
		TLSClientConfig:     tlsConfig,
		TLSHandshakeTimeout: tlsHandshakeTimeout,
		MaxIdleConns:        1,
		MaxIdleConnsPerHost: 1,
	}
	return &Client{origin: origin, http: &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("TURN provider redirect is prohibited")
		},
	}}, nil
}

func certificateAllowsClientAuthentication(certificate *x509.Certificate, now time.Time) bool {
	if now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) {
		return false
	}
	return slices.Contains(certificate.ExtKeyUsage, x509.ExtKeyUsageClientAuth)
}

func (client *Client) Acquire(
	ctx context.Context,
	request browsermatrixbroker.TURNAcquireRequest,
) (browsermatrixbroker.TURNReservation, error) {
	wire := acquireRequest{
		ProtocolVersion: ProtocolVersion, Operation: "acquire",
		RequestID: request.RequestID, RunID: request.RunID, ProfileID: request.ProfileID,
		ProbeNonce: request.ProbeNonce, ExpiresAt: request.ExpiresAt, MaxAttempts: request.MaxAttempts,
	}
	var response acquireResponse
	if err := client.exchange(ctx, acquirePath, wire, &response); err != nil {
		return browsermatrixbroker.TURNReservation{}, err
	}
	reservation := browsermatrixbroker.TURNReservation{
		ProviderLeaseID: response.ProviderLeaseID, RequestID: response.RequestID,
		RunID: response.RunID, ProfileID: response.ProfileID, ProbeNonce: response.ProbeNonce,
		CredentialID: response.CredentialID, Username: response.Username,
		ExpiresAt: response.ExpiresAt, MaxAttempts: response.MaxAttempts,
		Credential: response.Credential,
	}
	if response.ProtocolVersion != ProtocolVersion || response.Operation != "acquire" ||
		reservation.RequestID != request.RequestID || reservation.RunID != request.RunID ||
		reservation.ProfileID != request.ProfileID || reservation.ProbeNonce != request.ProbeNonce ||
		reservation.ExpiresAt != request.ExpiresAt || reservation.MaxAttempts != request.MaxAttempts {
		erase(reservation.Credential)
		return browsermatrixbroker.TURNReservation{}, errors.New("TURN provider acquire response changed its authority")
	}
	return reservation, nil
}

func (client *Client) BindAndWait(
	ctx context.Context,
	request browsermatrixbroker.TURNBindRequest,
) (browsermatrixbroker.TURNBoundLease, error) {
	wire := bindRequest{
		ProtocolVersion: ProtocolVersion, Operation: "bind-and-wait",
		ProviderLeaseID: request.ProviderLeaseID, RequestID: request.RequestID,
		RunID: request.RunID, ProfileID: request.ProfileID, ProbeNonce: request.ProbeNonce,
		ControlLeaseID: request.ControlLeaseID, AttestationSHA256: request.AttestationSHA256,
		CredentialID: request.CredentialID, Username: request.Username,
		ExpiresAt: request.ExpiresAt, MaxAttempts: request.MaxAttempts,
	}
	var response bindResponse
	if err := client.exchange(ctx, bindPath, wire, &response); err != nil {
		return browsermatrixbroker.TURNBoundLease{}, err
	}
	if response.ProtocolVersion != ProtocolVersion || response.Operation != "bind-and-wait" ||
		response.ProviderLeaseID != request.ProviderLeaseID || response.RequestID != request.RequestID ||
		response.RunID != request.RunID || response.ProfileID != request.ProfileID ||
		response.ProbeNonce != request.ProbeNonce || response.ControlLeaseID != request.ControlLeaseID ||
		response.AttestationSHA256 != request.AttestationSHA256 || response.CredentialID != request.CredentialID ||
		response.Username != request.Username || response.ExpiresAt != request.ExpiresAt ||
		response.MaxAttempts != request.MaxAttempts {
		return browsermatrixbroker.TURNBoundLease{}, errors.New("TURN provider bind response changed its authority")
	}
	return browsermatrixbroker.TURNBoundLease(request), nil
}

func (client *Client) RevokeAndWait(
	ctx context.Context,
	request browsermatrixbroker.TURNRetirementRequest,
) (browsermatrixbroker.TURNRetirementReceipt, error) {
	wire := revokeRequest{
		ProtocolVersion: ProtocolVersion, Operation: "revoke-and-wait",
		RetirementReason: request.Operation, RequestID: request.RequestID,
		ProviderLeaseID: request.ProviderLeaseID, ControlLeaseID: request.ControlLeaseID,
		RunID: request.RunID, ProfileID: request.ProfileID, ProbeNonce: request.ProbeNonce,
		AttestationSHA256: request.AttestationSHA256,
	}
	var response revokeResponse
	if err := client.exchange(ctx, revokePath, wire, &response); err != nil {
		return browsermatrixbroker.TURNRetirementReceipt{}, err
	}
	if response.ProtocolVersion != ProtocolVersion || response.Operation != "revoke-and-wait" ||
		response.RequestID != request.RequestID || response.ProviderLeaseID != request.ProviderLeaseID ||
		response.Terminal != "revoked" {
		return browsermatrixbroker.TURNRetirementReceipt{}, errors.New("TURN provider revocation response changed its authority")
	}
	return browsermatrixbroker.TURNRetirementReceipt{
		RequestID: response.RequestID, ProviderLeaseID: response.ProviderLeaseID,
		Terminal: response.Terminal,
	}, nil
}

func (client *Client) exchange(ctx context.Context, path string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return errors.New("TURN provider request encoding failed")
	}
	endpoint := *client.origin
	endpoint.Path = path
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return errors.New("TURN provider request construction failed")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return errors.New("TURN provider request failed")
	}
	defer response.Body.Close() //nolint:errcheck // The bounded response verdict owns this short-lived body.
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "application/json" ||
		response.Header.Get("Location") != "" {
		return errors.New("TURN provider rejected the request")
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseBytes+1))
	if err != nil || len(encoded) == 0 || len(encoded) > maximumResponseBytes {
		erase(encoded)
		return errors.New("TURN provider response is unavailable")
	}
	defer erase(encoded)
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("TURN provider response is invalid")
	}
	canonical, err := json.Marshal(output)
	if err != nil {
		return errors.New("TURN provider response is invalid")
	}
	defer erase(canonical)
	if !bytes.Equal(encoded, canonical) {
		return errors.New("TURN provider response is not canonical JSON")
	}
	return nil
}

func parseOrigin(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" ||
		value != fmt.Sprintf("https://%s/", parsed.Host) {
		return nil, errors.New("TURN provider origin is invalid")
	}
	return parsed, nil
}

func erase(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
