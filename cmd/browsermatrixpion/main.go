package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/windshare/windshare/internal/browsermatrixbroker"
	"github.com/windshare/windshare/internal/browsermatrixpion"
)

const (
	minimumControlBytes           = 32
	maximumUDPPort                = 1<<16 - 1
	canonicalUTCMillisecondLayout = "2006-01-02T15:04:05.000Z"
)

type commandConfig struct {
	listenAddress           string
	fixture                 browsermatrixpion.ExternalFixture
	attestationSigner       ed25519.PrivateKey
	publicIP                string
	controllerPublicIP      string
	udpPortMin              uint16
	udpPortMax              uint16
	credential              []byte
	certificate             tls.Certificate
	maximumLease            time.Duration
	attemptStartTimeout     time.Duration
	offerTimeout            time.Duration
	probeTimeout            time.Duration
	bodyReadTimeout         time.Duration
	tombstoneRetention      time.Duration
	maximumActive           int
	maximumTombstones       int
	serverReadHeaderTimeout time.Duration
	serverReadTimeout       time.Duration
	serverWriteTimeout      time.Duration
	serverIdleTimeout       time.Duration
	shutdownTimeout         time.Duration
	trace                   browsermatrixpion.TraceSink
	brokerPolicy            *browsermatrixbroker.ServerPolicy
}

type commandArguments struct {
	config                    commandConfig
	udpPortMin                uint
	udpPortMax                uint
	credentialFile            string
	certificateFile           string
	privateKeyFile            string
	attestationTemplateFile   string
	attestationPrivateKeyFile string
	brokerPolicyFile          string
}

func main() {
	os.Exit(commandExitCode(os.Args[1:]))
}

func commandExitCode(arguments []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	config, err := parseCommand(arguments)
	if err != nil {
		log.Print("remote Pion configuration rejected")
		return 2
	}
	defer erase(config.credential)
	defer erase(config.attestationSigner)
	config.trace = jsonTraceSink(os.Stderr)
	if err := run(ctx, config); err != nil {
		log.Print("remote Pion authority terminated unsuccessfully")
		return 1
	}
	return 0
}

func parseCommand(arguments []string) (commandConfig, error) {
	parsed, err := parseCommandArguments(arguments)
	if err != nil {
		return commandConfig{}, err
	}
	return loadCommandConfig(parsed)
}

func parseCommandArguments(arguments []string) (commandArguments, error) {
	flags := flag.NewFlagSet("browsermatrixpion", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var parsed commandArguments
	bindCommandFlags(flags, &parsed)
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return commandArguments{}, errors.New("remote Pion arguments are invalid")
	}
	if !commandArgumentsAreComplete(parsed) {
		return commandArguments{}, errors.New("remote Pion arguments are incomplete")
	}
	if !commandHTTPTimeoutsAreCoherent(parsed.config) {
		return commandArguments{}, errors.New("remote Pion HTTP timeout authority is incoherent")
	}
	return parsed, nil
}

func bindCommandFlags(flags *flag.FlagSet, parsed *commandArguments) {
	config := &parsed.config
	flags.StringVar(&config.listenAddress, "listen", "", "required TLS listen address")
	flags.StringVar(&parsed.attestationTemplateFile, "attestation-template-file", "", "required canonical external fixture declaration")
	flags.StringVar(&parsed.attestationPrivateKeyFile, "attestation-private-key-file", "", "required Ed25519 PKCS#8 attestation private key")
	flags.StringVar(&parsed.brokerPolicyFile, "credential-broker-policy-file", "", "optional canonical OIDC broker policy")
	flags.StringVar(&config.publicIP, "public-ip", "", "required authorized public IPv4 address")
	flags.StringVar(&config.controllerPublicIP, "controller-public-ip", "", "required externally observed controller IPv4 address")
	flags.UintVar(&parsed.udpPortMin, "udp-port-min", 0, "required ICE UDP range start")
	flags.UintVar(&parsed.udpPortMax, "udp-port-max", 0, "required ICE UDP range end")
	flags.StringVar(&parsed.credentialFile, "credential-file", "", "required bearer credential file")
	flags.StringVar(&parsed.certificateFile, "tls-certificate-file", "", "required TLS certificate file")
	flags.StringVar(&parsed.privateKeyFile, "tls-private-key-file", "", "required TLS private key file")
	flags.DurationVar(&config.maximumLease, "maximum-lease", 0, "required maximum attempt lease")
	flags.DurationVar(&config.attemptStartTimeout, "attempt-start-timeout", 0, "required attempt construction deadline")
	flags.DurationVar(&config.offerTimeout, "offer-timeout", 0, "required SDP offer deadline")
	flags.DurationVar(&config.probeTimeout, "probe-timeout", 0, "required STUN probe deadline")
	flags.DurationVar(&config.bodyReadTimeout, "body-read-timeout", 0, "required request body deadline")
	flags.DurationVar(&config.tombstoneRetention, "tombstone-retention", 0, "required idempotency retention")
	flags.IntVar(&config.maximumActive, "maximum-active", 0, "required maximum concurrent attempts")
	flags.IntVar(&config.maximumTombstones, "maximum-tombstones", 0, "required bounded tombstone capacity")
	flags.DurationVar(&config.serverReadHeaderTimeout, "server-read-header-timeout", 0, "required HTTP header deadline")
	flags.DurationVar(&config.serverReadTimeout, "server-read-timeout", 0, "required HTTP request deadline")
	flags.DurationVar(&config.serverWriteTimeout, "server-write-timeout", 0, "required HTTP response deadline")
	flags.DurationVar(&config.serverIdleTimeout, "server-idle-timeout", 0, "required HTTP idle deadline")
	flags.DurationVar(&config.shutdownTimeout, "shutdown-timeout", 0, "required graceful shutdown deadline")
}

func commandArgumentsAreComplete(parsed commandArguments) bool {
	config := parsed.config
	return config.listenAddress != "" && config.publicIP != "" && config.controllerPublicIP != "" &&
		parsed.attestationTemplateFile != "" && parsed.attestationPrivateKeyFile != "" &&
		parsed.udpPortMin > 0 && parsed.udpPortMin <= maximumUDPPort &&
		parsed.udpPortMax >= parsed.udpPortMin && parsed.udpPortMax <= maximumUDPPort &&
		parsed.credentialFile != "" && parsed.certificateFile != "" && parsed.privateKeyFile != "" &&
		config.maximumLease > 0 && config.attemptStartTimeout > 0 && config.offerTimeout > 0 &&
		config.probeTimeout > 0 && config.bodyReadTimeout > 0 && config.tombstoneRetention > 0 &&
		config.maximumActive > 0 && config.maximumTombstones >= config.maximumActive &&
		config.serverReadHeaderTimeout > 0 && config.serverReadTimeout > 0 &&
		config.serverWriteTimeout > 0 && config.serverIdleTimeout > 0 && config.shutdownTimeout > 0
}

func commandHTTPTimeoutsAreCoherent(config commandConfig) bool {
	maximumHandlerTime := max(config.attemptStartTimeout, config.offerTimeout, config.probeTimeout)
	return config.serverReadTimeout >= config.serverReadHeaderTimeout+config.bodyReadTimeout &&
		config.serverWriteTimeout >= config.bodyReadTimeout+maximumHandlerTime
}

func loadCommandConfig(parsed commandArguments) (commandConfig, error) {
	credential, err := readCredential(parsed.credentialFile)
	if err != nil {
		return commandConfig{}, err
	}
	var attestationSigner ed25519.PrivateKey
	accepted := false
	defer func() {
		if !accepted {
			erase(credential)
			erase(attestationSigner)
		}
	}()

	certificate, err := tls.LoadX509KeyPair(parsed.certificateFile, parsed.privateKeyFile)
	if err != nil {
		return commandConfig{}, errors.New("remote Pion TLS identity is invalid")
	}
	fixture, err := loadExternalFixture(parsed.attestationTemplateFile)
	if err != nil {
		return commandConfig{}, err
	}
	if err := validateFixtureCredentialExpiry(fixture); err != nil {
		return commandConfig{}, err
	}
	attestationSigner, err = readAttestationPrivateKey(parsed.attestationPrivateKeyFile)
	if err != nil {
		return commandConfig{}, err
	}
	if err := validateExternalFixtureAuthority(fixture, certificate, attestationSigner, parsed); err != nil {
		return commandConfig{}, err
	}
	brokerPolicy, err := loadBrokerPolicy(parsed.brokerPolicyFile, fixture)
	if err != nil {
		return commandConfig{}, err
	}
	if fixture.ProfileID == "scheduled-coturn" {
		return commandConfig{}, errors.New("coturn fixture lacks a concrete revocable provider capability")
	}

	config := parsed.config
	config.fixture = fixture
	config.attestationSigner = attestationSigner
	config.udpPortMin = fixture.RemotePeerUDPPortMin
	config.udpPortMax = fixture.RemotePeerUDPPortMax
	config.credential = credential
	config.certificate = certificate
	config.brokerPolicy = brokerPolicy
	accepted = true
	return config, nil
}

func loadExternalFixture(path string) (browsermatrixpion.ExternalFixture, error) {
	fixtureDocument, err := readBoundedFile(path, 1<<20)
	if err != nil {
		return browsermatrixpion.ExternalFixture{}, errors.New("external fixture attestation template is unavailable")
	}
	defer erase(fixtureDocument)
	fixture, err := browsermatrixpion.ParseCanonicalExternalFixture(fixtureDocument)
	if err != nil {
		return browsermatrixpion.ExternalFixture{}, errors.New("external fixture attestation template is invalid")
	}
	return fixture, nil
}

func validateFixtureCredentialExpiry(fixture browsermatrixpion.ExternalFixture) error {
	if fixture.ProfileID != "scheduled-coturn" {
		return nil
	}
	credentialExpiry, err := time.Parse(
		canonicalUTCMillisecondLayout,
		fixture.NetworkSemantics.TURNCredentialExpiresAt,
	)
	if err != nil || !time.Now().UTC().Before(credentialExpiry) {
		return errors.New("external fixture TURN credential declaration is expired")
	}
	return nil
}

func validateExternalFixtureAuthority(
	fixture browsermatrixpion.ExternalFixture,
	certificate tls.Certificate,
	attestationSigner ed25519.PrivateKey,
	parsed commandArguments,
) error {
	executableDigest, err := currentExecutableSHA256()
	if err != nil || fixture.ImplementationSHA256 != executableDigest ||
		fixture.TLSCertificateSHA256 != tlsLeafSHA256(certificate) ||
		!tlsCertificateAuthorizesOrigin(certificate, fixture.ControllerOrigin, time.Now().UTC()) ||
		!attestationKeyIsIndependentOfTLS(attestationSigner, certificate) ||
		fixture.ControllerPublicIP != parsed.config.controllerPublicIP || fixture.RemotePeerPublicIP != parsed.config.publicIP ||
		fixture.RemotePeerUDPPortMin != uint16(parsed.udpPortMin) || fixture.RemotePeerUDPPortMax != uint16(parsed.udpPortMax) {
		return errors.New("external fixture declaration does not describe this process")
	}
	return nil
}

func loadBrokerPolicy(path string, fixture browsermatrixpion.ExternalFixture) (*browsermatrixbroker.ServerPolicy, error) {
	if path == "" {
		return nil, nil
	}
	policy, err := browsermatrixbroker.LoadServerPolicy(path)
	if err != nil || policy.ControllerOrigin != fixture.ControllerOrigin || policy.ProfileID != fixture.ProfileID {
		return nil, errors.New("credential broker policy does not describe this fixture")
	}
	return &policy, nil
}

func readCredential(path string) ([]byte, error) {
	value, err := os.ReadFile(path)
	if err != nil || len(value) < minimumControlBytes || len(value) > 512 || !bytes.Equal(bytes.TrimSpace(value), value) {
		erase(value)
		return nil, errors.New("remote Pion control credential is invalid")
	}
	for _, character := range value {
		if !credentialCharacterIsSafe(character) {
			erase(value)
			return nil, errors.New("remote Pion control credential is invalid")
		}
	}
	return value, nil
}

func credentialCharacterIsSafe(character byte) bool {
	return character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' ||
		character >= '0' && character <= '9' || character == '_' || character == '-'
}

func run(ctx context.Context, config commandConfig) (resultErr error) {
	factory, err := browsermatrixpion.NewPionAttemptFactory(browsermatrixpion.PionAttemptFactoryConfig{
		InstanceID: config.fixture.RemoteServiceInstanceID, PublicIP: config.fixture.RemotePeerPublicIP,
		UDPPortMin: config.udpPortMin, UDPPortMax: config.udpPortMax, Trace: config.trace,
	})
	if err != nil {
		return errors.New("remote Pion peer factory rejected its authority")
	}
	service, err := browsermatrixpion.NewService(browsermatrixpion.ServiceConfig{
		Fixture: config.fixture, AttestationSigner: config.attestationSigner,
		MaximumLease: config.maximumLease, Credential: config.credential,
		AttemptStartTimeout: config.attemptStartTimeout, OfferTimeout: config.offerTimeout,
		ProbeTimeout: config.probeTimeout, BodyReadTimeout: config.bodyReadTimeout,
		TombstoneRetention: config.tombstoneRetention, MaximumActive: config.maximumActive,
		MaximumTombstones: config.maximumTombstones,
		AttemptFactory:    factory, STUNProber: browsermatrixpion.RealSTUNProber{},
		Trace: config.trace,
	})
	if err != nil {
		return errors.New("remote Pion service rejected its authority")
	}
	var broker *browsermatrixbroker.Handler
	contained := false
	defer func() {
		if contained {
			return
		}
		// Setup failures still own every authority already constructed. Broker must
		// settle its child leases before the underlying Pion authority disappears.
		resultErr = errors.Join(resultErr, closeBrokerAuthority(broker, config.shutdownTimeout), service.Close())
	}()
	handler := http.Handler(service)
	if config.brokerPolicy != nil {
		policy := *config.brokerPolicy
		broker, err = browsermatrixbroker.NewHandler(browsermatrixbroker.Config{
			ControllerOrigin: policy.ControllerOrigin, ProfileID: policy.ProfileID,
			ExpectedIdentity:   policy.ExpectedWorkloadIdentity(),
			LeaseDuration:      time.Duration(policy.LeaseMillis) * time.Millisecond,
			RetirementTimeout:  time.Duration(policy.RetirementTimeoutMillis) * time.Millisecond,
			TombstoneRetention: time.Duration(policy.TombstoneRetentionMillis) * time.Millisecond,
			MaximumTombstones:  policy.MaximumTombstones,
			MaximumOIDCReplays: policy.MaximumOIDCReplays,
			Signer:             config.attestationSigner, Admin: service,
			Trace: brokerTraceSink(os.Stderr),
		})
		if err != nil {
			return errors.New("credential broker service rejected its authority")
		}
		handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if browsermatrixbroker.TargetsBrokerEndpoint(request) {
				broker.ServeHTTP(writer, request)
				return
			}
			service.ServeHTTP(writer, request)
		})
	}

	listener, err := net.Listen("tcp", config.listenAddress)
	if err != nil {
		return errors.New("remote Pion TLS listener failed")
	}
	tlsListener := tls.NewListener(listener, &tls.Config{
		Certificates: []tls.Certificate{config.certificate}, MinVersion: tls.VersionTLS13,
	})
	httpServer := &http.Server{
		Handler: handler, ReadHeaderTimeout: config.serverReadHeaderTimeout,
		ReadTimeout: config.serverReadTimeout, WriteTimeout: config.serverWriteTimeout,
		IdleTimeout: config.serverIdleTimeout,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	serveResult := make(chan error, 1)
	go func() { serveResult <- httpServer.Serve(tlsListener) }()

	var serveErr error
	select {
	case <-ctx.Done():
		serveErr = shutdownHTTPServer(httpServer, config.shutdownTimeout)
		terminalErr := <-serveResult
		if terminalErr == nil || !errors.Is(terminalErr, http.ErrServerClosed) {
			serveErr = errors.Join(serveErr, errors.New("remote Pion TLS server failed"))
		}
	case <-serveResult:
		serveErr = errors.New("remote Pion TLS server terminated unexpectedly")
		if closeTransportErr := httpServer.Close(); closeTransportErr != nil {
			serveErr = errors.Join(serveErr, errors.New("remote Pion active connections did not close"))
		}
	}
	brokerCloseErr := closeBrokerAuthority(broker, config.shutdownTimeout)
	closeErr := service.Close()
	contained = true
	if serveErr != nil || brokerCloseErr != nil || closeErr != nil {
		return errors.Join(
			errors.New("remote Pion containment did not complete cleanly"),
			serveErr, brokerCloseErr, closeErr,
		)
	}
	return nil
}

func closeBrokerAuthority(broker *browsermatrixbroker.Handler, timeout time.Duration) error {
	if broker == nil {
		return nil
	}
	closeContext, cancelClose := context.WithTimeout(context.Background(), timeout)
	closeErr := broker.CloseAndWait(closeContext)
	cancelClose()
	if closeErr == nil {
		return nil
	}
	forceContext, cancelForce := context.WithTimeout(context.Background(), timeout)
	forceErr := broker.ForceCloseAndWait(forceContext)
	cancelForce()
	return errors.Join(closeErr, forceErr)
}

type httpShutdownAuthority interface {
	Shutdown(context.Context) error
	Close() error
}

func shutdownHTTPServer(server httpShutdownAuthority, timeout time.Duration) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	shutdownErr := server.Shutdown(shutdownCtx)
	cancel()
	if shutdownErr == nil {
		return nil
	}
	// Shutdown does not force-close live connections after its context expires.
	// The command owns the listener and every accepted connection, so it must
	// retire them before reporting the failed graceful phase.
	closeErr := server.Close()
	return errors.Join(
		errors.New("remote Pion graceful shutdown deadline exceeded"),
		closeErr,
	)
}

func erase(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func jsonTraceSink(writer io.Writer) browsermatrixpion.TraceSink {
	logger := log.New(writer, "", 0)
	return func(event browsermatrixpion.TraceEvent) {
		encoded, err := json.Marshal(event)
		if err == nil {
			logger.Print(string(encoded))
		}
	}
}

func brokerTraceSink(writer io.Writer) browsermatrixbroker.TraceSink {
	logger := log.New(writer, "", 0)
	return func(event browsermatrixbroker.TraceEvent) {
		encoded, err := json.Marshal(event)
		if err == nil {
			logger.Print(string(encoded))
		}
	}
}

func (config commandConfig) String() string {
	return fmt.Sprintf("browsermatrixpion(instance=%s,profile=%s,udp=%s-%s)",
		config.fixture.RemoteServiceInstanceID, config.fixture.ProfileID,
		strconv.FormatUint(uint64(config.udpPortMin), 10), strconv.FormatUint(uint64(config.udpPortMax), 10))
}

func readBoundedFile(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close() //nolint:errcheck // Read failure owns the verdict; the process owns this short-lived descriptor.
	value, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || len(value) == 0 || int64(len(value)) > maximum {
		erase(value)
		return nil, errors.New("bounded authority file is invalid")
	}
	return value, nil
}

func readAttestationPrivateKey(path string) (ed25519.PrivateKey, error) {
	document, err := readBoundedFile(path, 4096)
	if err != nil {
		return nil, errors.New("external fixture attestation private key is unavailable")
	}
	defer erase(document)
	block, rest := pem.Decode(document)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("external fixture attestation private key is invalid")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("external fixture attestation private key is invalid")
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("external fixture attestation private key is invalid")
	}
	return append(ed25519.PrivateKey(nil), privateKey...), nil
}

func currentExecutableSHA256() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close() //nolint:errcheck // Hashing failure owns the startup verdict.
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func tlsLeafSHA256(certificate tls.Certificate) string {
	if len(certificate.Certificate) == 0 {
		return ""
	}
	digest := sha256.Sum256(certificate.Certificate[0])
	return hex.EncodeToString(digest[:])
}

func tlsCertificateAuthorizesOrigin(certificate tls.Certificate, origin string, now time.Time) bool {
	if len(certificate.Certificate) == 0 {
		return false
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil || now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return false
	}
	endpoint, err := url.Parse(origin)
	return err == nil && leaf.VerifyHostname(endpoint.Hostname()) == nil
}

func attestationKeyIsIndependentOfTLS(privateKey ed25519.PrivateKey, certificate tls.Certificate) bool {
	if len(privateKey) != ed25519.PrivateKeySize || len(certificate.Certificate) == 0 {
		return false
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return false
	}
	tlsPublicKey, sameAlgorithm := leaf.PublicKey.(ed25519.PublicKey)
	return !sameAlgorithm || !bytes.Equal(privateKey.Public().(ed25519.PublicKey), tlsPublicKey)
}
