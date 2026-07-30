package browsermatrixbroker

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
	"os"
	"path/filepath"
	"time"
)

const (
	maximumClientConfigBytes = 64 << 10
	maximumClientTimeout     = time.Minute
)

type ClientConfig struct {
	SchemaVersion               string `json:"schemaVersion"`
	ServerOrigin                string `json:"serverOrigin"`
	TLSCertificateAuthorityFile string `json:"tlsCertificateAuthorityFile"`
	TLSCertificateSHA256        string `json:"tlsCertificateSha256"`
	OperationTimeoutMillis      int64  `json:"operationTimeoutMillis"`
}

type Client struct {
	config     ClientConfig
	endpoint   string
	httpClient *http.Client
}

func LoadClientConfig(path string) (ClientConfig, error) {
	if !canonicalAbsolutePath(path) {
		return ClientConfig{}, errors.New("credential broker client config path is invalid")
	}
	document, err := os.ReadFile(path)
	if err != nil || len(document) == 0 || len(document) > maximumClientConfigBytes {
		erase(document)
		return ClientConfig{}, errors.New("credential broker client config is unavailable")
	}
	defer erase(document)
	var config ClientConfig
	if !decodeCanonicalLine(document, &config) || validateClientConfig(config) != nil {
		return ClientConfig{}, errors.New("credential broker client config is invalid")
	}
	return config, nil
}

func NewClient(config ClientConfig) (*Client, error) {
	if err := validateClientConfig(config); err != nil {
		return nil, err
	}
	certificateAuthority, err := os.ReadFile(config.TLSCertificateAuthorityFile)
	if err != nil || len(certificateAuthority) == 0 || len(certificateAuthority) > MaximumFrameBytes {
		erase(certificateAuthority)
		return nil, errors.New("credential broker TLS certificate authority is unavailable")
	}
	defer erase(certificateAuthority)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificateAuthority) {
		return nil, errors.New("credential broker TLS certificate authority is invalid")
	}
	endpoint, _ := url.Parse(config.ServerOrigin)
	expectedDigest, _ := hex.DecodeString(config.TLSCertificateSHA256)
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: endpoint.Hostname(),
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("credential broker TLS peer certificate is unavailable")
			}
			digest := sha256.Sum256(state.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare(digest[:], expectedDigest) != 1 {
				return errors.New("credential broker TLS peer certificate pin is invalid")
			}
			return nil
		},
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   time.Duration(config.OperationTimeoutMillis) * time.Millisecond,
			KeepAlive: -1,
		}).DialContext,
		ForceAttemptHTTP2:  true,
		DisableCompression: true,
		TLSClientConfig:    tlsConfig,
	}
	return newClientWithHTTPClient(config, &http.Client{Transport: transport}), nil
}

// NewTestClientForHarness admits an injected transport without weakening the
// production constructor's CA and leaf-certificate pins.
func NewTestClientForHarness(config ClientConfig, client *http.Client) (*Client, error) {
	if err := validateClientConfig(config); err != nil || client == nil {
		return nil, errors.New("credential broker test client is invalid")
	}
	return newClientWithHTTPClient(config, client), nil
}

func newClientWithHTTPClient(config ClientConfig, client *http.Client) *Client {
	isolated := *client
	// A 307/308 redirect would replay the assertion-bearing frame byte-for-byte.
	// Rejecting every redirect keeps the pinned origin as the sole dispatcher.
	isolated.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{
		config: config, endpoint: config.ServerOrigin[:len(config.ServerOrigin)-1] + BrokerPath,
		httpClient: &isolated,
	}
}

func (client *Client) Exchange(ctx context.Context, input io.Reader, output io.Writer) error {
	if client == nil || client.httpClient == nil || input == nil || output == nil {
		return errors.New("credential broker client is incomplete")
	}
	frame, err := readBoundedFrame(input)
	if err != nil {
		return err
	}
	defer erase(frame)
	parsed, err := parseRequestFrame(frame)
	if err != nil || validateRequestShape(parsed) != nil || parsed.scope.ControllerOrigin != client.config.ServerOrigin {
		return ErrInvalidFrame
	}
	operationContext, cancel := context.WithTimeout(
		ctx,
		time.Duration(client.config.OperationTimeoutMillis)*time.Millisecond,
	)
	defer cancel()
	request, err := http.NewRequestWithContext(operationContext, http.MethodPost, client.endpoint, bytes.NewReader(frame))
	if err != nil {
		return errors.New("credential broker client request is invalid")
	}
	request.ContentLength = int64(len(frame))
	request.Header.Set("Content-Type", BrokerContentType)
	request.Header.Set("Accept", BrokerContentType)
	response, err := client.httpClient.Do(request)
	if err != nil {
		return errors.New("credential broker client request failed")
	}
	defer response.Body.Close() //nolint:errcheck // A failed exact read owns the transport verdict.
	if response.StatusCode != http.StatusOK || response.TLS == nil || response.Request == nil ||
		response.Request.URL == nil || response.Request.URL.String() != client.endpoint ||
		response.Header.Get("Content-Type") != BrokerContentType || response.Header.Get("Content-Encoding") != "" ||
		response.ContentLength <= 0 || response.ContentLength > MaximumFrameBytes {
		return errors.New("credential broker server response is invalid")
	}
	result, err := io.ReadAll(io.LimitReader(response.Body, MaximumFrameBytes+1))
	if err != nil || len(result) == 0 || len(result) > MaximumFrameBytes ||
		int64(len(result)) != response.ContentLength {
		erase(result)
		return errors.New("credential broker server response is incomplete")
	}
	defer erase(result)
	if err := writeAll(output, result); err != nil {
		return errors.New("credential broker client stdout failed")
	}
	return nil
}

func (client *Client) Close() {
	if client == nil || client.httpClient == nil {
		return
	}
	client.httpClient.CloseIdleConnections()
}

func (client Client) String() string {
	return fmt.Sprintf("credential-broker-client(server=%s)", client.config.ServerOrigin)
}

func validateClientConfig(config ClientConfig) error {
	timeout := time.Duration(config.OperationTimeoutMillis) * time.Millisecond
	if config.SchemaVersion != ClientConfigSchemaVersion || !canonicalControllerOrigin(config.ServerOrigin) ||
		!canonicalAbsolutePath(config.TLSCertificateAuthorityFile) ||
		!sha256Pattern.MatchString(config.TLSCertificateSHA256) || timeout <= 0 ||
		timeout > maximumClientTimeout {
		return errors.New("credential broker client config is invalid")
	}
	return nil
}

func decodeCanonicalLine(document []byte, destination any) bool {
	if len(document) < 2 || document[len(document)-1] != '\n' ||
		bytes.Contains(document[:len(document)-1], []byte{'\n'}) || bytes.Contains(document, []byte{'\r'}) ||
		json.Unmarshal(document[:len(document)-1], destination) != nil {
		return false
	}
	canonical, err := canonicalJSONLine(destination)
	return err == nil && bytes.Equal(document, canonical)
}

func canonicalAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}

func writeAll(writer io.Writer, value []byte) error {
	for len(value) != 0 {
		written, err := writer.Write(value)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(value) {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}
