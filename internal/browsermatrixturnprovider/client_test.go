package browsermatrixturnprovider

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/browsermatrixbroker"
)

const (
	testRequestID       = "turn-request-00000001"
	testProviderLeaseID = "turn-provider-lease-00000001"
	testRunID           = "scheduled-run"
	testProbeNonce      = "probe-nonce-00000001"
	testControlLeaseID  = "control-lease-00000001"
	testCredentialID    = "turn-credential-00000001"
	testUsername        = "turn-user-00000001"
	testExpiresAt       = "2031-02-03T04:05:06.000Z"
)

type httpDoerFunc func(*http.Request) (*http.Response, error)

func (do httpDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return do(request)
}

func TestClientExchangesExactProviderContract(t *testing.T) {
	origin, err := url.Parse("https://turn-provider.example/")
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{origin: origin, http: httpDoerFunc(func(request *http.Request) (*http.Response, error) {
		var output any
		switch request.URL.Path {
		case acquirePath:
			output = acquireResponse{
				ProtocolVersion: ProtocolVersion, Operation: "acquire",
				ProviderLeaseID: testProviderLeaseID, RequestID: testRequestID,
				RunID: testRunID, ProfileID: "scheduled-coturn", ProbeNonce: testProbeNonce,
				CredentialID: testCredentialID, Username: testUsername,
				ExpiresAt: testExpiresAt, MaxAttempts: 1,
				Credential: []byte("turn_secret_AAAAAAAAAAAAAAAAAAAAA"),
			}
		case bindPath:
			output = bindResponse{
				ProtocolVersion: ProtocolVersion, Operation: "bind-and-wait",
				ProviderLeaseID: testProviderLeaseID, RequestID: testRequestID,
				RunID: testRunID, ProfileID: "scheduled-coturn", ProbeNonce: testProbeNonce,
				ControlLeaseID: testControlLeaseID, AttestationSHA256: strings.Repeat("a", 64),
				CredentialID: testCredentialID, Username: testUsername,
				ExpiresAt: testExpiresAt, MaxAttempts: 1,
			}
		case revokePath:
			output = revokeResponse{
				ProtocolVersion: ProtocolVersion, Operation: "revoke-and-wait",
				RequestID: testRequestID, ProviderLeaseID: testProviderLeaseID, Terminal: "revoked",
			}
		default:
			t.Fatalf("unexpected provider path %q", request.URL.Path)
		}
		encoded, marshalErr := json.Marshal(output)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       ioNopCloser{Reader: strings.NewReader(string(encoded))},
		}, nil
	})}

	reservation, err := client.Acquire(context.Background(), testAcquireRequest())
	if err != nil {
		t.Fatal(err)
	}
	if reservation.ProviderLeaseID != testProviderLeaseID ||
		string(reservation.Credential) != "turn_secret_AAAAAAAAAAAAAAAAAAAAA" {
		t.Fatalf("unexpected reservation: %s", reservation)
	}
	bound, err := client.BindAndWait(context.Background(), testBindRequest())
	if err != nil || bound.ProviderLeaseID != testProviderLeaseID {
		t.Fatalf("unexpected bound lease: %#v err=%v", bound, err)
	}
	receipt, err := client.RevokeAndWait(context.Background(), testRetirementRequest())
	if err != nil || receipt.Terminal != "revoked" {
		t.Fatalf("unexpected retirement receipt: %#v err=%v", receipt, err)
	}
}

func TestClientRejectsNonCanonicalOrChangedAuthority(t *testing.T) {
	valid := acquireResponse{
		ProtocolVersion: ProtocolVersion, Operation: "acquire",
		ProviderLeaseID: testProviderLeaseID, RequestID: testRequestID,
		RunID: testRunID, ProfileID: "scheduled-coturn", ProbeNonce: testProbeNonce,
		CredentialID: testCredentialID, Username: testUsername,
		ExpiresAt: testExpiresAt, MaxAttempts: 1,
		Credential: []byte("turn_secret_AAAAAAAAAAAAAAAAAAAAA"),
	}
	canonical, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	for name, response := range map[string]func() (*http.Response, error){
		"transport": func() (*http.Response, error) { return nil, errors.New("secret transport") },
		"status": func() (*http.Response, error) {
			return testHTTPResponse(http.StatusServiceUnavailable, "application/json", canonical), nil
		},
		"content-type": func() (*http.Response, error) {
			return testHTTPResponse(http.StatusOK, "text/plain", canonical), nil
		},
		"unknown": func() (*http.Response, error) {
			return testHTTPResponse(http.StatusOK, "application/json", append(canonical[:len(canonical)-1], []byte(",\"extra\":true}")...)), nil
		},
		"non-canonical": func() (*http.Response, error) {
			return testHTTPResponse(http.StatusOK, "application/json", append([]byte{'\n'}, canonical...)), nil
		},
		"oversized": func() (*http.Response, error) {
			return testHTTPResponse(http.StatusOK, "application/json", []byte(strings.Repeat("x", maximumResponseBytes+1))), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			origin, parseErr := url.Parse("https://turn-provider.example/")
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			client := &Client{origin: origin, http: httpDoerFunc(func(*http.Request) (*http.Response, error) {
				return response()
			})}
			if _, acquireErr := client.Acquire(context.Background(), testAcquireRequest()); acquireErr == nil ||
				strings.Contains(acquireErr.Error(), "secret") {
				t.Fatalf("invalid response accepted or reflected: %v", acquireErr)
			}
		})
	}

	changed := valid
	changed.ExpiresAt = "2031-02-03T04:05:07.000Z"
	client := testClientReturning(t, changed)
	if _, err := client.Acquire(context.Background(), testAcquireRequest()); err == nil {
		t.Fatal("provider response changed the requested lease")
	}
}

func TestNewClientUsesMutualTLSAndPinnedServerIdentity(t *testing.T) {
	authority := newTestTLSAuthority(t)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != acquirePath || request.TLS == nil || len(request.TLS.PeerCertificates) == 0 {
			http.Error(writer, "invalid authority", http.StatusBadRequest)
			return
		}
		encoded, err := json.Marshal(acquireResponse{
			ProtocolVersion: ProtocolVersion, Operation: "acquire",
			ProviderLeaseID: testProviderLeaseID, RequestID: testRequestID,
			RunID: testRunID, ProfileID: "scheduled-coturn", ProbeNonce: testProbeNonce,
			CredentialID: testCredentialID, Username: testUsername,
			ExpiresAt: testExpiresAt, MaxAttempts: 1,
			Credential: []byte("turn_secret_AAAAAAAAAAAAAAAAAAAAA"),
		})
		if err != nil {
			t.Fatal(err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(encoded)
	}))
	server.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{authority.server},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    authority.roots,
	}
	server.StartTLS()
	defer server.Close()

	digest := sha256.Sum256(authority.server.Certificate[0])
	client, err := New(Config{
		Origin: server.URL + "/", TLSCertificateAuthority: authority.rootPEM,
		ServerCertificateSHA256: hex.EncodeToString(digest[:]),
		ClientCertificate:       authority.client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Acquire(context.Background(), testAcquireRequest()); err != nil {
		t.Fatal(err)
	}

	wrongDigest := strings.Repeat("0", sha256.Size*2)
	untrusted, err := New(Config{
		Origin: server.URL + "/", TLSCertificateAuthority: authority.rootPEM,
		ServerCertificateSHA256: wrongDigest, ClientCertificate: authority.client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := untrusted.Acquire(context.Background(), testAcquireRequest()); err == nil {
		t.Fatal("unpinned provider identity was accepted")
	}
}

func TestNewClientRejectsIncompleteTrust(t *testing.T) {
	for name, config := range map[string]Config{
		"origin": {Origin: "http://provider.example/"},
		"digest": {Origin: "https://provider.example/", ServerCertificateSHA256: "invalid"},
		"authority": {
			Origin: "https://provider.example/", ServerCertificateSHA256: strings.Repeat("0", 64),
			TLSCertificateAuthority: []byte("invalid"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(config); err == nil {
				t.Fatal("incomplete provider trust accepted")
			}
		})
	}
}

type ioNopCloser struct {
	*strings.Reader
}

func (ioNopCloser) Close() error { return nil }

func testHTTPResponse(status int, contentType string, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}},
		Body: ioNopCloser{Reader: strings.NewReader(string(body))},
	}
}

func testClientReturning(t *testing.T, value any) *Client {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := url.Parse("https://turn-provider.example/")
	if err != nil {
		t.Fatal(err)
	}
	return &Client{origin: origin, http: httpDoerFunc(func(*http.Request) (*http.Response, error) {
		return testHTTPResponse(http.StatusOK, "application/json", encoded), nil
	})}
}

func testAcquireRequest() browsermatrixbroker.TURNAcquireRequest {
	return browsermatrixbroker.TURNAcquireRequest{
		RequestID: testRequestID, RunID: testRunID, ProfileID: "scheduled-coturn",
		ProbeNonce: testProbeNonce, ExpiresAt: testExpiresAt, MaxAttempts: 1,
	}
}

func testBindRequest() browsermatrixbroker.TURNBindRequest {
	return browsermatrixbroker.TURNBindRequest{
		ProviderLeaseID: testProviderLeaseID, RequestID: testRequestID,
		RunID: testRunID, ProfileID: "scheduled-coturn", ProbeNonce: testProbeNonce,
		ControlLeaseID: testControlLeaseID, AttestationSHA256: strings.Repeat("a", 64),
		CredentialID: testCredentialID, Username: testUsername,
		ExpiresAt: testExpiresAt, MaxAttempts: 1,
	}
}

func testRetirementRequest() browsermatrixbroker.TURNRetirementRequest {
	return browsermatrixbroker.TURNRetirementRequest{
		Operation: "revoke-and-wait", RequestID: testRequestID,
		ProviderLeaseID: testProviderLeaseID, ControlLeaseID: testControlLeaseID,
		RunID: testRunID, ProfileID: "scheduled-coturn", ProbeNonce: testProbeNonce,
		AttestationSHA256: strings.Repeat("a", 64),
	}
}

type testTLSAuthority struct {
	rootPEM []byte
	roots   *x509.CertPool
	server  tls.Certificate
	client  tls.Certificate
}

func newTestTLSAuthority(t *testing.T) testTLSAuthority {
	t.Helper()
	rootPublic, rootPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rootTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test root"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, rootPublic, rootPrivate)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	rootPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})
	roots := x509.NewCertPool()
	roots.AddCert(root)
	return testTLSAuthority{
		rootPEM: rootPEM, roots: roots,
		server: issueTestCertificate(t, root, rootPrivate, 2, true),
		client: issueTestCertificate(t, root, rootPrivate, 3, false),
	}
}

func issueTestCertificate(
	t *testing.T,
	root *x509.Certificate,
	rootPrivate ed25519.PrivateKey,
	serial int64,
	server bool,
) tls.Certificate {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "provider identity"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	if server {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, root, public, rootPrivate)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}
