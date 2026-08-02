package browsermatrixbroker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientRejects307And308WithoutDispatchingRedirectTarget(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		var targetRequests atomic.Int64
		target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			targetRequests.Add(1)
		}))
		defer target.Close()
		redirect := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Location", target.URL+BrokerPath)
			writer.WriteHeader(status)
		}))
		defer redirect.Close()
		config := testClientConfig(redirect.URL + "/")
		client, err := NewTestClientForHarness(config, redirect.Client())
		if err != nil {
			t.Fatal(err)
		}
		input := clientAcquireFrame(t, config.ServerOrigin)
		var output bytes.Buffer
		if err := client.Exchange(context.Background(), bytes.NewReader(input), &output); err == nil {
			t.Fatalf("redirect status %d was accepted", status)
		}
		if targetRequests.Load() != 0 || output.Len() != 0 {
			t.Fatalf("redirect status %d dispatched target=%d output=%d", status, targetRequests.Load(), output.Len())
		}
		erase(input)
		client.Close()
	}
}

func TestProductionClientPinsTLSAndReadsExactEOF(t *testing.T) {
	responseCredential := bytes.Repeat([]byte{'C'}, minimumCredentialBytes)
	responseFrame, err := encodeLeaseFrame(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize)), LeasePayload{
		ProtocolVersion: RemotePionProtocolVersion, RequestID: testAcquireRequestID,
		ReleaseRequestID: testReleaseRequestID, RevokeRequestID: testRevokeRequestID,
		LeaseID: "control-lease-00000001", RunID: testRunID, ProfileID: "scheduled-public-stun",
		ProbeNonce: testProbeNonce, AuthorityInstanceID: "remote-authority",
		AttestationSHA256: string(bytes.Repeat([]byte{'a'}, 64)),
		IssuedAt:          "2031-02-03T04:05:06.000Z", ExpiresAt: "2031-02-03T04:06:06.000Z",
		MaxAttempts: 1, CredentialByteLength: len(responseCredential), TURNCapability: "not-required",
	}, responseCredential)
	if err != nil {
		t.Fatal(err)
	}
	defer erase(responseCredential)
	defer erase(responseFrame)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != BrokerPath || request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", BrokerContentType)
		writer.Header().Set("Content-Length", strconv.Itoa(len(responseFrame)))
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(responseFrame)
	}))
	defer server.Close()
	certificateFile := filepath.Join(t.TempDir(), "broker-ca.pem")
	certificate := server.Certificate()
	if err := os.WriteFile(certificateFile, pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: certificate.Raw,
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(certificate.Raw)
	config := testClientConfig(server.URL + "/")
	config.TLSCertificateAuthorityFile = certificateFile
	config.TLSCertificateSHA256 = hex.EncodeToString(digest[:])
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	input := clientAcquireFrame(t, config.ServerOrigin)
	defer erase(input)
	var output bytes.Buffer
	if err := client.Exchange(context.Background(), bytes.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), responseFrame) {
		t.Fatal("production client changed the exact binary frame")
	}
	erase(output.Bytes())

	config.TLSCertificateSHA256 = string(bytes.Repeat([]byte{'0'}, 64))
	hostile, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	defer hostile.Close()
	if err := hostile.Exchange(context.Background(), bytes.NewReader(input), &bytes.Buffer{}); err == nil {
		t.Fatal("client accepted a different TLS leaf pin")
	}
}

func testClientConfig(origin string) ClientConfig {
	return ClientConfig{
		SchemaVersion: ClientConfigSchemaVersion, ServerOrigin: origin,
		TLSCertificateAuthorityFile: `C:\broker\ca.pem`,
		TLSCertificateSHA256:        string(bytes.Repeat([]byte{'a'}, 64)),
		OperationTimeoutMillis:      int64((5 * time.Second) / time.Millisecond),
	}
}

func clientAcquireFrame(t *testing.T, origin string) []byte {
	t.Helper()
	metadata := acquireMetadata{
		SchemaVersion: ProtocolVersion, Operation: "acquire", RequestID: testAcquireRequestID,
		ReleaseRequestID: testReleaseRequestID, RevokeRequestID: testRevokeRequestID,
		ControllerOrigin: origin, RunID: testRunID, ProfileID: "scheduled-public-stun",
		ProbeNonce: testProbeNonce, MaxAttempts: 1, WorkloadIdentity: testIdentity(),
		WorkloadIdentityByteLength: len("workload-assertion"),
	}
	return requestFrame(t, metadata)
}
