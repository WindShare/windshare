package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/browsermatrixbroker"
	"github.com/windshare/windshare/internal/testnetwork"
)

func TestExecuteUsesFixedConfigAndExactAnonymousPipes(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	response := []byte("authenticated-public-proof")
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Method != http.MethodPost || request.URL.Path != browsermatrixbroker.BrokerPath {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", browsermatrixbroker.BrokerContentType)
		writer.Header().Set("Content-Length", strconv.Itoa(len(response)))
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(response)
	}))
	defer server.Close()
	directory := t.TempDir()
	writeClientFixture(t, directory, server)
	input := acquireFrame(t, server.URL+"/")
	var output bytes.Buffer
	var errorOutput bytes.Buffer
	code := execute(
		context.Background(),
		[]string{"browsermatrixbrokerclient"},
		func() (string, error) { return directory, nil },
		bytes.NewReader(input),
		&output,
		&errorOutput,
	)
	if code != 0 || requests.Load() != 1 || !bytes.Equal(output.Bytes(), response) || errorOutput.Len() != 0 {
		t.Fatalf(
			"exact exchange code=%d requests=%d output=%q stderr=%q",
			code, requests.Load(), output.Bytes(), errorOutput.Bytes(),
		)
	}
}

func TestExecuteRejectsArgumentsConfigurationAndExchangeFailures(t *testing.T) {
	tests := []struct {
		name      string
		arguments []string
		getwd     func() (string, error)
		input     []byte
		wantCode  int
		wantError string
	}{
		{
			name: "arguments", arguments: []string{"client", "hostile"},
			getwd:    func() (string, error) { t.Fatal("arguments reached configuration"); return "", nil },
			wantCode: 2, wantError: "credential broker client arguments rejected\n",
		},
		{
			name: "working directory", arguments: []string{"client"},
			getwd:    func() (string, error) { return "", errors.New("unavailable") },
			wantCode: 2, wantError: "credential broker client configuration rejected\n",
		},
		{
			name: "configuration", arguments: []string{"client"},
			getwd:    func() (string, error) { return t.TempDir(), nil },
			wantCode: 2, wantError: "credential broker client configuration rejected\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := execute(
				context.Background(), test.arguments, test.getwd,
				bytes.NewReader(test.input), &bytes.Buffer{}, &stderr,
			)
			if code != test.wantCode || stderr.String() != test.wantError {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
		})
	}

	directory := t.TempDir()
	certificateFile := filepath.Join(directory, "ca.pem")
	if err := os.WriteFile(certificateFile, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeCanonicalConfig(t, directory, browsermatrixbroker.ClientConfig{
		SchemaVersion:               browsermatrixbroker.ClientConfigSchemaVersion,
		ServerOrigin:                "https://broker.example/",
		TLSCertificateAuthorityFile: certificateFile,
		TLSCertificateSHA256:        string(bytes.Repeat([]byte{'a'}, 64)),
		OperationTimeoutMillis:      int64(time.Second / time.Millisecond),
	})
	var stderr bytes.Buffer
	code := execute(
		context.Background(), []string{"client"}, func() (string, error) { return directory, nil },
		bytes.NewReader([]byte("invalid frame")), &bytes.Buffer{}, &stderr,
	)
	if code != 1 || stderr.String() != "credential broker client exchange failed\n" {
		t.Fatalf("exchange failure code=%d stderr=%q", code, stderr.String())
	}
}

func writeClientFixture(t *testing.T, directory string, server *httptest.Server) {
	t.Helper()
	certificate := server.Certificate()
	certificateFile := filepath.Join(directory, "broker-ca.pem")
	if err := os.WriteFile(certificateFile, pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: certificate.Raw,
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(certificate.Raw)
	writeCanonicalConfig(t, directory, browsermatrixbroker.ClientConfig{
		SchemaVersion:               browsermatrixbroker.ClientConfigSchemaVersion,
		ServerOrigin:                server.URL + "/",
		TLSCertificateAuthorityFile: certificateFile,
		TLSCertificateSHA256:        hex.EncodeToString(digest[:]),
		OperationTimeoutMillis:      int64((5 * time.Second) / time.Millisecond),
	})
}

func writeCanonicalConfig(t *testing.T, directory string, config browsermatrixbroker.ClientConfig) {
	t.Helper()
	document, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	document = append(document, '\n')
	if err := os.WriteFile(
		filepath.Join(directory, browsermatrixbroker.ClientConfigFileName),
		document,
		0o600,
	); err != nil {
		t.Fatal(err)
	}
}

func acquireFrame(t *testing.T, controllerOrigin string) []byte {
	t.Helper()
	const assertion = "workload-assertion"
	metadata, err := json.Marshal(struct {
		SchemaVersion              string                                      `json:"schemaVersion"`
		Operation                  string                                      `json:"operation"`
		RequestID                  string                                      `json:"requestId"`
		ReleaseRequestID           string                                      `json:"releaseRequestId"`
		RevokeRequestID            string                                      `json:"revokeRequestId"`
		ControllerOrigin           string                                      `json:"controllerOrigin"`
		RunID                      string                                      `json:"runId"`
		ProfileID                  string                                      `json:"profileId"`
		ProbeNonce                 string                                      `json:"probeNonce"`
		MaxAttempts                int                                         `json:"maxAttempts"`
		WorkloadIdentity           browsermatrixbroker.WorkloadIdentityBinding `json:"workloadIdentity"`
		WorkloadIdentityByteLength int                                         `json:"workloadIdentityByteLength"`
	}{
		SchemaVersion: browsermatrixbroker.ProtocolVersion, Operation: "acquire",
		RequestID: "acquire-request-00000001", ReleaseRequestID: "release-request-00000001",
		RevokeRequestID: "revoke-request-00000001", ControllerOrigin: controllerOrigin,
		RunID: "scheduled-run", ProfileID: "scheduled-public-stun", ProbeNonce: "probe-nonce-00000001",
		MaxAttempts: 1,
		WorkloadIdentity: browsermatrixbroker.WorkloadIdentityBinding{
			ProtocolVersion: browsermatrixbroker.WorkloadIdentityProtocolVersion,
			Kind:            "github-actions-oidc", Audience: "windshare-browser-matrix",
			Issuer: "https://token.actions.githubusercontent.com", Repository: "windshare/windshare",
			Ref:           "refs/heads/main",
			WorkflowRef:   "windshare/windshare/.github/workflows/network.yml@refs/heads/main",
			RequestOrigin: "https://github.example", RequestPath: "/oidc",
			RequestQuery: "?audience=windshare",
		},
		WorkloadIdentityByteLength: len(assertion),
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata = append(metadata, '\n')
	frame := make([]byte, 4+len(metadata)+len(assertion))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(metadata)))
	copy(frame[4:], metadata)
	copy(frame[4+len(metadata):], assertion)
	return frame
}
