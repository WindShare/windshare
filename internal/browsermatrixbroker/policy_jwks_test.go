package browsermatrixbroker

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/testnetwork"
)

func TestCanonicalPolicyAndClientConfigLoading(t *testing.T) {
	directory := t.TempDir()
	clientPath := filepath.Join(directory, ClientConfigFileName)
	clientConfig := ClientConfig{
		SchemaVersion:               ClientConfigSchemaVersion,
		ServerOrigin:                testControllerOrigin,
		TLSCertificateAuthorityFile: filepath.Join(directory, "broker-ca.pem"),
		TLSCertificateSHA256:        string(bytes.Repeat([]byte{'a'}, 64)),
		OperationTimeoutMillis:      int64((5 * time.Second) / time.Millisecond),
	}
	writeCanonicalTestDocument(t, clientPath, clientConfig)
	loadedClient, err := LoadClientConfig(clientPath)
	if err != nil || loadedClient != clientConfig {
		t.Fatalf("canonical client config was not preserved: config=%+v err=%v", loadedClient, err)
	}
	if _, err := LoadClientConfig(filepath.Join(directory, "missing-client.json")); err == nil {
		t.Fatal("missing client config was accepted")
	}

	policyPath := filepath.Join(directory, "broker-policy.json")
	policy := validServerPolicy()
	writeCanonicalTestDocument(t, policyPath, policy)
	loadedPolicy, err := LoadServerPolicy(policyPath)
	if err != nil || loadedPolicy != policy || loadedPolicy.ExpectedWorkloadIdentity() != testIdentity() {
		t.Fatalf("canonical server policy was not preserved: policy=%+v err=%v", loadedPolicy, err)
	}

	policy.ProfileID = "manual-real-nat"
	writeCanonicalTestDocument(t, policyPath, policy)
	if _, err := LoadServerPolicy(policyPath); err == nil {
		t.Fatal("broker policy admitted the operator-owned manual topology")
	}
	if _, err := LoadServerPolicy("broker-policy.json"); err == nil {
		t.Fatal("relative broker policy path was accepted")
	}
}

func TestConfigLoadersRejectNoncanonicalDocuments(t *testing.T) {
	directory := t.TempDir()
	clientPath := filepath.Join(directory, ClientConfigFileName)
	canonical, err := canonicalJSONLine(ClientConfig{
		SchemaVersion:               ClientConfigSchemaVersion,
		ServerOrigin:                testControllerOrigin,
		TLSCertificateAuthorityFile: filepath.Join(directory, "broker-ca.pem"),
		TLSCertificateSHA256:        string(bytes.Repeat([]byte{'b'}, 64)),
		OperationTimeoutMillis:      int64(time.Second / time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	noncanonical := append([]byte{' '}, canonical...)
	if err := os.WriteFile(clientPath, noncanonical, 0o600); err != nil {
		t.Fatal(err)
	}
	erase(noncanonical)
	erase(canonical)
	if _, err := LoadClientConfig(clientPath); err == nil {
		t.Fatal("noncanonical client config was accepted")
	}
}

func TestHTTPJWKSFetcherPinsExactTLSResponseAndRejectsRedirects(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	document := []byte(`{"keys":[]}`)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet ||
			request.Header.Get("Accept") != "application/jwk-set+json, application/json" {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/jwk-set+json")
		_, _ = writer.Write(document)
	}))
	defer server.Close()
	fetcher := NewHTTPJWKSFetcher(server.Client())
	got, err := fetcher.Fetch(context.Background(), server.URL+"/jwks")
	if err != nil || !bytes.Equal(got, document) {
		t.Fatalf("exact JWKS response was not preserved: document=%q err=%v", got, err)
	}
	erase(got)

	var targetRequests atomic.Int64
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetRequests.Add(1)
	}))
	defer target.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", target.URL+"/jwks")
		writer.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	if _, err := NewHTTPJWKSFetcher(redirect.Client()).Fetch(context.Background(), redirect.URL+"/jwks"); err == nil {
		t.Fatal("JWKS redirect was accepted")
	}
	if targetRequests.Load() != 0 {
		t.Fatal("JWKS redirect target received authority-bearing request")
	}
	if _, err := (*HTTPJWKSFetcher)(nil).Fetch(context.Background(), "http://hostile.example/jwks"); err == nil {
		t.Fatal("non-TLS JWKS endpoint was accepted")
	}
	if NewHTTPJWKSFetcher(nil).client.Timeout != defaultJWKSRequestTimeout {
		t.Fatal("default JWKS fetcher omitted its bounded request deadline")
	}
}

func validServerPolicy() ServerPolicy {
	return ServerPolicy{
		SchemaVersion: ServerPolicySchemaVersion, ControllerOrigin: testControllerOrigin,
		ProfileID: "scheduled-public-stun", Audience: testIdentity().Audience,
		Issuer: GitHubActionsOIDCIssuer, Repository: testIdentity().Repository,
		Ref: testIdentity().Ref, WorkflowRef: testIdentity().WorkflowRef,
		IdentityRequestOrigin:    testIdentity().RequestOrigin,
		IdentityRequestPath:      testIdentity().RequestPath,
		IdentityRequestQuery:     testIdentity().RequestQuery,
		LeaseMillis:              int64(time.Minute / time.Millisecond),
		RetirementTimeoutMillis:  int64(time.Second / time.Millisecond),
		TombstoneRetentionMillis: int64(time.Hour / time.Millisecond),
		MaximumTombstones:        32, MaximumOIDCReplays: 32,
	}
}

func writeCanonicalTestDocument(t *testing.T, path string, value any) {
	t.Helper()
	document, err := canonicalJSONLine(value)
	if err != nil {
		t.Fatal(err)
	}
	defer erase(document)
	if err := os.WriteFile(path, document, 0o600); err != nil {
		t.Fatal(err)
	}
}
