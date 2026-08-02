package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/browsermatrixbroker"
	"github.com/windshare/windshare/internal/browsermatrixpion"
)

const commandTestCredential = "command_control_AAAAAAAAAAAAAAAAAAAAAAAAA"

func TestParseCommandRequiresExplicitSecretAndTopologyAuthority(t *testing.T) {
	directory := t.TempDir()
	credentialPath := filepath.Join(directory, "control")
	certificatePath, keyPath := writeTestTLSIdentity(t, directory)
	attestationPath, attestationKeyPath := writeTestAttestationAuthority(
		t, directory, certificatePath, keyPath, 43000, 43010,
	)
	if err := os.WriteFile(credentialPath, []byte(commandTestCredential), 0o600); err != nil {
		t.Fatal(err)
	}
	arguments := []string{
		"-listen=127.0.0.1:0", "-public-ip=1.1.1.1", "-controller-public-ip=8.8.8.8",
		"-attestation-template-file=" + attestationPath,
		"-attestation-private-key-file=" + attestationKeyPath,
		"-udp-port-min=43000", "-udp-port-max=43010", "-credential-file=" + credentialPath,
		"-tls-certificate-file=" + certificatePath, "-tls-private-key-file=" + keyPath,
		"-maximum-lease=30s", "-attempt-start-timeout=2s", "-offer-timeout=5s",
		"-probe-timeout=3s", "-body-read-timeout=2s", "-tombstone-retention=1m",
		"-maximum-active=4", "-maximum-tombstones=16",
		"-server-read-header-timeout=2s", "-server-read-timeout=4s",
		"-server-write-timeout=8s", "-server-idle-timeout=10s", "-shutdown-timeout=2s",
	}
	config, err := parseCommand(arguments)
	if err != nil {
		t.Fatal(err)
	}
	if string(config.credential) != commandTestCredential || config.udpPortMin != 43000 ||
		config.maximumLease != 30*time.Second || config.maximumActive != 4 ||
		config.serverReadTimeout != 4*time.Second || config.fixture.ProfileID != "scheduled-public-stun" ||
		len(config.attestationSigner) != ed25519.PrivateKeySize {
		t.Fatalf("parsed configuration is incomplete: %s", config)
	}
	if strings.Contains(config.String(), commandTestCredential) {
		t.Fatal("configuration representation leaked control credential")
	}
	erase(config.credential)
	erase(config.attestationSigner)
	for _, value := range config.credential {
		if value != 0 {
			t.Fatal("credential erasure failed")
		}
	}

	for name, invalid := range map[string][]string{
		"missing":          nil,
		"unknown":          append(append([]string{}, arguments...), "-unknown=true"),
		"positional":       append(append([]string{}, arguments...), "extra"),
		"range":            replaceArgument(arguments, "-udp-port-max=43010", "-udp-port-max=42000"),
		"timeout":          replaceArgument(arguments, "-offer-timeout=5s", "-offer-timeout=0s"),
		"envelope":         replaceArgument(arguments, "-server-write-timeout=8s", "-server-write-timeout=6s"),
		"capacity":         replaceArgument(arguments, "-maximum-tombstones=16", "-maximum-tombstones=3"),
		"endpoint-binding": replaceArgument(arguments, "-public-ip=1.1.1.1", "-public-ip=9.9.9.9"),
		"template": replaceArgument(arguments, "-attestation-template-file="+attestationPath,
			"-attestation-template-file="+filepath.Join(directory, "missing-attestation")),
		"attestation-key": replaceArgument(arguments, "-attestation-private-key-file="+attestationKeyPath,
			"-attestation-private-key-file="+credentialPath),
		"collapsed-tls-attestation-key": replaceArgument(arguments,
			"-attestation-private-key-file="+attestationKeyPath,
			"-attestation-private-key-file="+keyPath),
		"unexpected-turn-credential": append(append([]string{}, arguments...),
			"-turn-credential-file="+credentialPath),
		"credential":  replaceArgument(arguments, "-credential-file="+credentialPath, "-credential-file="+filepath.Join(directory, "missing")),
		"certificate": replaceArgument(arguments, "-tls-certificate-file="+certificatePath, "-tls-certificate-file="+credentialPath),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseCommand(invalid); err == nil || strings.Contains(err.Error(), commandTestCredential) {
				t.Fatalf("invalid arguments accepted or leaked secret: %v", err)
			}
		})
	}
}

func TestReadCredentialRejectsWhitespaceAndUnsafeAlphabet(t *testing.T) {
	for name, value := range map[string]string{
		"short":     "short",
		"newline":   commandTestCredential + "\n",
		"space":     strings.Repeat("x", 31) + " ",
		"oversized": strings.Repeat("x", 513),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credential")
			if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readCredential(path); err == nil || strings.Contains(err.Error(), value) {
				t.Fatalf("unsafe credential accepted or reflected: %v", err)
			}
		})
	}
}

func TestParseCommandRejectsCoturnWithoutRevocableProvider(t *testing.T) {
	directory := t.TempDir()
	certificatePath, tlsKeyPath := writeTestTLSIdentity(t, directory)
	expiresAt := time.Now().UTC().Truncate(time.Millisecond).Add(4 * time.Minute).
		Format(canonicalUTCMillisecondLayout)
	attestationPath, attestationKeyPath := writeTestAttestationAuthorityProfile(
		t, directory, certificatePath, tlsKeyPath, 43500, 43510,
		"scheduled-coturn", expiresAt,
	)
	credentialPath := writeCredential(t, directory)
	arguments := []string{
		"-listen=127.0.0.1:0", "-public-ip=1.1.1.1", "-controller-public-ip=8.8.8.8",
		"-attestation-template-file=" + attestationPath,
		"-attestation-private-key-file=" + attestationKeyPath,
		"-udp-port-min=43500", "-udp-port-max=43510", "-credential-file=" + credentialPath,
		"-tls-certificate-file=" + certificatePath, "-tls-private-key-file=" + tlsKeyPath,
		"-maximum-lease=5m", "-attempt-start-timeout=2s", "-offer-timeout=5s",
		"-probe-timeout=3s", "-body-read-timeout=2s", "-tombstone-retention=1m",
		"-maximum-active=4", "-maximum-tombstones=16",
		"-server-read-header-timeout=2s", "-server-read-timeout=4s",
		"-server-write-timeout=8s", "-server-idle-timeout=10s", "-shutdown-timeout=2s",
	}
	if _, err := parseCommand(arguments); err == nil {
		t.Fatal("Coturn declaration started without a revocable provider capability")
	}
	policyDocument, err := json.Marshal(browsermatrixbroker.ServerPolicy{
		SchemaVersion:    browsermatrixbroker.ServerPolicySchemaVersion,
		ControllerOrigin: "https://matrix.local:8443/", ProfileID: "scheduled-coturn",
		Audience: "windshare-browser-matrix", Issuer: browsermatrixbroker.GitHubActionsOIDCIssuer,
		Repository: "windshare/windshare", Ref: "refs/heads/main",
		WorkflowRef:           "windshare/windshare/.github/workflows/network.yml@refs/heads/main",
		IdentityRequestOrigin: "https://github.example", IdentityRequestPath: "/oidc",
		IdentityRequestQuery: "?audience=windshare", LeaseMillis: 60_000,
		RetirementTimeoutMillis: 30_000, TombstoneRetentionMillis: 60_000,
		MaximumTombstones: 16, MaximumOIDCReplays: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	policyPath := filepath.Join(directory, "credential-broker-policy.json")
	if err := os.WriteFile(policyPath, append(policyDocument, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseCommand(append(
		append([]string(nil), arguments...), "-credential-broker-policy-file="+policyPath,
	)); err == nil {
		t.Fatal("OIDC policy was mistaken for a concrete revocable TURN provider")
	}
	providerCertificate, err := tls.LoadX509KeyPair(certificatePath, tlsKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	clientCertificatePath, clientKeyPath := writeTestClientTLSIdentity(t, directory)
	providerArguments := append(append([]string(nil), arguments...),
		"-credential-broker-policy-file="+policyPath,
		"-turn-provider-origin=https://matrix.local:9443/",
		"-turn-provider-tls-ca-file="+certificatePath,
		"-turn-provider-server-certificate-sha256="+tlsLeafSHA256(providerCertificate),
		"-turn-provider-client-certificate-file="+clientCertificatePath,
		"-turn-provider-client-key-file="+clientKeyPath,
	)
	configured, err := parseCommand(providerArguments)
	if err != nil || configured.turnProvider == nil {
		t.Fatalf("concrete revocable TURN provider was not composed: %v", err)
	}
	if _, err := parseCommand(append(
		append([]string(nil), arguments...), "-turn-credential-file="+credentialPath,
	)); err == nil {
		t.Fatal("retired static TURN credential flag was accepted")
	}

	expiredDirectory := t.TempDir()
	expiredCertificate, expiredTLSKey := writeTestTLSIdentity(t, expiredDirectory)
	expiredTemplate, expiredAttestationKey := writeTestAttestationAuthorityProfile(
		t, expiredDirectory, expiredCertificate, expiredTLSKey, 43600, 43610,
		"scheduled-coturn", time.Now().UTC().Add(-time.Second).Truncate(time.Millisecond).
			Format(canonicalUTCMillisecondLayout),
	)
	expiredCredential := writeCredential(t, expiredDirectory)
	expiredArguments := append([]string(nil), arguments...)
	for oldValue, newValue := range map[string]string{
		"-attestation-template-file=" + attestationPath:       "-attestation-template-file=" + expiredTemplate,
		"-attestation-private-key-file=" + attestationKeyPath: "-attestation-private-key-file=" + expiredAttestationKey,
		"-tls-certificate-file=" + certificatePath:            "-tls-certificate-file=" + expiredCertificate,
		"-tls-private-key-file=" + tlsKeyPath:                 "-tls-private-key-file=" + expiredTLSKey,
		"-credential-file=" + credentialPath:                  "-credential-file=" + expiredCredential,
		"-udp-port-min=43500":                                 "-udp-port-min=43600",
		"-udp-port-max=43510":                                 "-udp-port-max=43610",
	} {
		expiredArguments = replaceArgument(expiredArguments, oldValue, newValue)
	}
	if _, err := parseCommand(expiredArguments); err == nil {
		t.Fatal("expired Coturn credential declaration was accepted")
	}
}

func TestRunStartsAndRetiresTLSAuthority(t *testing.T) {
	directory := t.TempDir()
	certificatePath, keyPath := writeTestTLSIdentity(t, directory)
	attestationPath, attestationKeyPath := writeTestAttestationAuthority(
		t, directory, certificatePath, keyPath, 44000, 44010,
	)
	certificate, err := parseCommand([]string{
		"-listen=127.0.0.1:0", "-public-ip=1.1.1.1", "-controller-public-ip=8.8.8.8",
		"-attestation-template-file=" + attestationPath,
		"-attestation-private-key-file=" + attestationKeyPath,
		"-udp-port-min=44000", "-udp-port-max=44010", "-credential-file=" + writeCredential(t, directory),
		"-tls-certificate-file=" + certificatePath, "-tls-private-key-file=" + keyPath,
		"-maximum-lease=10s", "-attempt-start-timeout=2s", "-offer-timeout=5s",
		"-probe-timeout=3s", "-body-read-timeout=2s", "-tombstone-retention=1m",
		"-maximum-active=4", "-maximum-tombstones=16",
		"-server-read-header-timeout=2s", "-server-read-timeout=4s",
		"-server-write-timeout=8s", "-server-idle-timeout=10s", "-shutdown-timeout=2s",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, certificate); err != nil {
		t.Fatal(err)
	}
	invalid := certificate
	invalid.fixture.RemotePeerPublicIP = "invalid"
	if err := run(context.Background(), invalid); err == nil {
		t.Fatal("invalid Pion endpoint authority started")
	}
	invalid = certificate
	invalid.listenAddress = "invalid-address"
	if err := run(context.Background(), invalid); err == nil {
		t.Fatal("invalid listener authority started")
	}
}

type fakeHTTPShutdownAuthority struct {
	shutdownErr error
	closeErr    error
	closed      bool
}

func (server *fakeHTTPShutdownAuthority) Shutdown(context.Context) error {
	return server.shutdownErr
}

func (server *fakeHTTPShutdownAuthority) Close() error {
	server.closed = true
	return server.closeErr
}

func TestShutdownHTTPServerForceClosesAfterGracefulFailure(t *testing.T) {
	server := &fakeHTTPShutdownAuthority{shutdownErr: context.DeadlineExceeded}
	if err := shutdownHTTPServer(server, time.Second); err == nil || !server.closed {
		t.Fatalf("graceful failure did not force-close server: closed=%v err=%v", server.closed, err)
	}
	server = &fakeHTTPShutdownAuthority{}
	if err := shutdownHTTPServer(server, time.Second); err != nil || server.closed {
		t.Fatalf("successful graceful shutdown was force-closed: closed=%v err=%v", server.closed, err)
	}
}

func replaceArgument(arguments []string, oldValue, newValue string) []string {
	result := append([]string(nil), arguments...)
	for index, argument := range result {
		if argument == oldValue {
			result[index] = newValue
			return result
		}
	}
	return result
}

func writeCredential(t *testing.T, directory string) string {
	t.Helper()
	path := filepath.Join(directory, "control")
	if err := os.WriteFile(path, []byte(commandTestCredential), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTestTLSIdentity(t *testing.T, directory string) (string, string) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "matrix.local"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"matrix.local"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificatePath := filepath.Join(directory, "tls.crt")
	keyPath := filepath.Join(directory, "tls.key")
	if err := os.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certificatePath, keyPath
}

func writeTestClientTLSIdentity(t *testing.T, directory string) (string, string) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "matrix provider client"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificatePath := filepath.Join(directory, "provider-client.crt")
	keyPath := filepath.Join(directory, "provider-client.key")
	if err := os.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certificatePath, keyPath
}

func writeTestAttestationAuthority(
	t *testing.T,
	directory string,
	certificatePath string,
	tlsKeyPath string,
	portMin uint16,
	portMax uint16,
) (string, string) {
	return writeTestAttestationAuthorityProfile(
		t, directory, certificatePath, tlsKeyPath, portMin, portMax,
		"scheduled-public-stun", "",
	)
}

func writeTestAttestationAuthorityProfile(
	t *testing.T,
	directory string,
	certificatePath string,
	tlsKeyPath string,
	portMin uint16,
	portMax uint16,
	profileID string,
	turnCredentialExpiresAt string,
) (string, string) {
	t.Helper()
	certificate, err := tls.LoadX509KeyPair(certificatePath, tlsKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	semantics := browsermatrixpion.NetworkSemantics{
		Kind:     browsermatrixpion.NetworkSemanticsPublicSTUN,
		PolicyID: "command-policy", PolicyVersion: 1,
		STUNEndpoint: "stun:stun.example:3478",
	}
	if profileID == "scheduled-coturn" {
		semantics = browsermatrixpion.NetworkSemantics{
			Kind:     browsermatrixpion.NetworkSemanticsCoturnRelay,
			PolicyID: "command-policy", PolicyVersion: 1,
			TURNServiceOwnerID: "command-turn-owner",
			TURNURLs:           []string{"turn:turn.example:3478?transport=udp"},
			TURNUsername:       "command-turn-user", TURNCredentialID: "command-turn-credential",
			TURNCredentialExpiresAt: turnCredentialExpiresAt,
		}
	}
	fixture := browsermatrixpion.ExternalFixture{
		SchemaVersion: browsermatrixpion.ExternalFixtureSchemaVersion,
		DeploymentID:  "command-deployment", Revision: 1,
		ProfileID: profileID, AuthorityInstanceID: "command-authority",
		RemoteServiceInstanceID: "remote-a",
		OperatorID:              "command-operator", FixtureHostID: "command-host",
		FixtureNetworkBoundaryID: "command-boundary",
		ControllerOrigin:         "https://matrix.local:8443/", ControllerPublicIP: "8.8.8.8",
		TLSCertificateSHA256: tlsLeafSHA256(certificate), RemotePeerPublicIP: "1.1.1.1",
		RemotePeerUDPPortMin: portMin, RemotePeerUDPPortMax: portMax,
		NetworkSemantics: semantics,
	}
	document, err := browsermatrixpion.CanonicalExternalFixtureDocument(fixture)
	if err != nil {
		t.Fatal(err)
	}
	attestationPath := filepath.Join(directory, fmt.Sprintf("fixture-%d.json", portMin))
	if err := os.WriteFile(attestationPath, document, 0o600); err != nil {
		t.Fatal(err)
	}
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	privateKeyPath := filepath.Join(directory, fmt.Sprintf("attestation-%d.key", portMin))
	if err := os.WriteFile(privateKeyPath, pem.EncodeToMemory(&pem.Block{
		Type: "PRIVATE KEY", Bytes: privateDER,
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	return attestationPath, privateKeyPath
}
