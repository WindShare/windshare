package browsermatrixbroker

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"time"
)

const (
	ProtocolVersion                 = "windshare.browser-network-matrix.credential-broker/v2"
	RemotePionProtocolVersion       = "windshare.browser-network-matrix.remote-pion/v2"
	WorkloadIdentityProtocolVersion = "windshare.browser-network-matrix.parent-workload-identity/v1"
	SignatureAlgorithm              = "ed25519"
	BrokerPath                      = "/v2/credential-broker"
	BrokerContentType               = "application/vnd.windshare.credential-broker-v2"
	ClientConfigSchemaVersion       = "windshare.browser-network-matrix.credential-broker-client-config/v1"
	ServerPolicySchemaVersion       = "windshare.browser-network-matrix.credential-broker-server-policy/v1"
	ClientConfigFileName            = "credential-broker-client.json"
	metadataLengthBytes             = 4
	MaximumFrameBytes               = 1 << 20
	minimumCredentialBytes          = 32
	maximumCredentialBytes          = 512
	canonicalTimestampLayout        = "2006-01-02T15:04:05.000Z"
)

var (
	canonicalIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
	opaqueIDPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
	sha256Pattern      = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

var ErrInvalidFrame = errors.New("credential broker frame is not canonical")

type WorkloadIdentityBinding struct {
	ProtocolVersion string `json:"protocolVersion"`
	Kind            string `json:"kind"`
	Audience        string `json:"audience"`
	Issuer          string `json:"issuer"`
	Repository      string `json:"repository"`
	Ref             string `json:"ref"`
	WorkflowRef     string `json:"workflowRef"`
	RequestOrigin   string `json:"requestOrigin"`
	RequestPath     string `json:"requestPath"`
	RequestQuery    string `json:"requestQuery"`
}

type acquireMetadata struct {
	SchemaVersion              string                  `json:"schemaVersion"`
	Operation                  string                  `json:"operation"`
	RequestID                  string                  `json:"requestId"`
	ReleaseRequestID           string                  `json:"releaseRequestId"`
	RevokeRequestID            string                  `json:"revokeRequestId"`
	ControllerOrigin           string                  `json:"controllerOrigin"`
	RunID                      string                  `json:"runId"`
	ProfileID                  string                  `json:"profileId"`
	ProbeNonce                 string                  `json:"probeNonce"`
	MaxAttempts                int                     `json:"maxAttempts"`
	WorkloadIdentity           WorkloadIdentityBinding `json:"workloadIdentity"`
	WorkloadIdentityByteLength int                     `json:"workloadIdentityByteLength"`
}

type retirementMetadata struct {
	SchemaVersion              string                  `json:"schemaVersion"`
	Operation                  string                  `json:"operation"`
	RequestID                  string                  `json:"requestId"`
	ControllerOrigin           string                  `json:"controllerOrigin"`
	LeaseID                    string                  `json:"leaseId"`
	RunID                      string                  `json:"runId"`
	ProfileID                  string                  `json:"profileId"`
	ProbeNonce                 string                  `json:"probeNonce"`
	WorkloadIdentity           WorkloadIdentityBinding `json:"workloadIdentity"`
	WorkloadIdentityByteLength int                     `json:"workloadIdentityByteLength"`
}

type requestScope struct {
	RequestID        string
	ReleaseRequestID string
	RevokeRequestID  string
	ControllerOrigin string
	RunID            string
	ProfileID        string
	ProbeNonce       string
	LeaseID          string
	Operation        string
	Identity         WorkloadIdentityBinding
}

type parsedRequest struct {
	scope             requestScope
	workloadAssertion []byte
}

type LeasePayload struct {
	ProtocolVersion      string `json:"protocolVersion"`
	RequestID            string `json:"requestId"`
	ReleaseRequestID     string `json:"releaseRequestId"`
	RevokeRequestID      string `json:"revokeRequestId"`
	LeaseID              string `json:"leaseId"`
	RunID                string `json:"runId"`
	ProfileID            string `json:"profileId"`
	ProbeNonce           string `json:"probeNonce"`
	AuthorityInstanceID  string `json:"authorityInstanceId"`
	AttestationSHA256    string `json:"attestationSha256"`
	IssuedAt             string `json:"issuedAt"`
	ExpiresAt            string `json:"expiresAt"`
	MaxAttempts          int    `json:"maxAttempts"`
	CredentialByteLength int    `json:"credentialByteLength"`
	TURNCapability       string `json:"turnCapability"`
	TURNProviderLeaseID  string `json:"turnProviderLeaseId"`
	TURNCredentialID     string `json:"turnCredentialId"`
	TURNUsername         string `json:"turnUsername"`
	TURNExpiresAt        string `json:"turnExpiresAt"`
}

type ReceiptPayload struct {
	ProtocolVersion     string `json:"protocolVersion"`
	Operation           string `json:"operation"`
	RequestID           string `json:"requestId"`
	ReleaseRequestID    string `json:"releaseRequestId"`
	RevokeRequestID     string `json:"revokeRequestId"`
	LeaseID             string `json:"leaseId"`
	RunID               string `json:"runId"`
	ProfileID           string `json:"profileId"`
	ProbeNonce          string `json:"probeNonce"`
	AuthorityInstanceID string `json:"authorityInstanceId"`
	AttestationSHA256   string `json:"attestationSha256"`
	LeaseExpiresAt      string `json:"leaseExpiresAt"`
	ControlTerminal     string `json:"controlTerminal"`
	TURNProviderLeaseID string `json:"turnProviderLeaseId"`
	TURNTerminal        string `json:"turnTerminal"`
	Terminal            string `json:"terminal"`
	RetiredAt           string `json:"retiredAt"`
}

type leaseEnvelope struct {
	ProtocolVersion    string       `json:"protocolVersion"`
	Lease              LeasePayload `json:"lease"`
	LeaseSHA256        string       `json:"leaseSha256"`
	SignatureAlgorithm string       `json:"signatureAlgorithm"`
	Signature          string       `json:"signature"`
}

type receiptEnvelope struct {
	ProtocolVersion    string         `json:"protocolVersion"`
	Receipt            ReceiptPayload `json:"receipt"`
	ReceiptSHA256      string         `json:"receiptSha256"`
	SignatureAlgorithm string         `json:"signatureAlgorithm"`
	Signature          string         `json:"signature"`
}

func parseRequestFrame(frame []byte) (parsedRequest, error) {
	metadata, payload, err := splitFrame(frame)
	if err != nil {
		return parsedRequest{}, err
	}
	var discriminator struct {
		Operation string `json:"operation"`
	}
	if json.Unmarshal(metadata, &discriminator) != nil {
		return parsedRequest{}, ErrInvalidFrame
	}
	switch discriminator.Operation {
	case "acquire":
		var value acquireMetadata
		if !decodeCanonicalMetadata(metadata, &value) || value.SchemaVersion != ProtocolVersion ||
			value.Operation != "acquire" || value.MaxAttempts != 1 ||
			value.WorkloadIdentityByteLength != len(payload) {
			return parsedRequest{}, ErrInvalidFrame
		}
		return parsedRequest{scope: requestScope{
			RequestID: value.RequestID, ReleaseRequestID: value.ReleaseRequestID,
			RevokeRequestID: value.RevokeRequestID, ControllerOrigin: value.ControllerOrigin,
			RunID: value.RunID, ProfileID: value.ProfileID, ProbeNonce: value.ProbeNonce,
			Operation: value.Operation, Identity: value.WorkloadIdentity,
		}, workloadAssertion: payload}, nil
	case "release", "revoke-and-wait":
		var value retirementMetadata
		if !decodeCanonicalMetadata(metadata, &value) || value.SchemaVersion != ProtocolVersion ||
			value.Operation != discriminator.Operation || value.WorkloadIdentityByteLength != len(payload) {
			return parsedRequest{}, ErrInvalidFrame
		}
		return parsedRequest{scope: requestScope{
			RequestID: value.RequestID, ControllerOrigin: value.ControllerOrigin,
			RunID: value.RunID, ProfileID: value.ProfileID, ProbeNonce: value.ProbeNonce,
			LeaseID: value.LeaseID, Operation: value.Operation, Identity: value.WorkloadIdentity,
		}, workloadAssertion: payload}, nil
	default:
		return parsedRequest{}, ErrInvalidFrame
	}
}

func validateRequestShape(request parsedRequest) error {
	if !validOpaqueID(request.scope.RequestID) || !canonicalIDPattern.MatchString(request.scope.RunID) ||
		!validProfileID(request.scope.ProfileID) || !validOpaqueID(request.scope.ProbeNonce) ||
		(request.scope.Operation == "acquire" && (!validOpaqueID(request.scope.ReleaseRequestID) ||
			!validOpaqueID(request.scope.RevokeRequestID) ||
			request.scope.RequestID == request.scope.ReleaseRequestID ||
			request.scope.RequestID == request.scope.RevokeRequestID ||
			request.scope.ReleaseRequestID == request.scope.RevokeRequestID)) ||
		(request.scope.Operation != "acquire" && !validOpaqueID(request.scope.LeaseID)) ||
		len(request.workloadAssertion) == 0 || len(request.workloadAssertion) > MaximumFrameBytes {
		return ErrInvalidFrame
	}
	return nil
}

func splitFrame(frame []byte) ([]byte, []byte, error) {
	if len(frame) <= metadataLengthBytes || len(frame) > MaximumFrameBytes {
		return nil, nil, ErrInvalidFrame
	}
	metadataLength := int(binary.BigEndian.Uint32(frame[:metadataLengthBytes]))
	payloadOffset := metadataLengthBytes + metadataLength
	if metadataLength == 0 || payloadOffset > len(frame) {
		return nil, nil, ErrInvalidFrame
	}
	metadataLine := frame[metadataLengthBytes:payloadOffset]
	if len(metadataLine) < 2 || metadataLine[len(metadataLine)-1] != '\n' ||
		bytes.Contains(metadataLine[:len(metadataLine)-1], []byte{'\n'}) || bytes.Contains(metadataLine, []byte{'\r'}) {
		return nil, nil, ErrInvalidFrame
	}
	return metadataLine[:len(metadataLine)-1], frame[payloadOffset:], nil
}

func decodeCanonicalMetadata(document []byte, destination any) bool {
	if !json.Valid(document) || json.Unmarshal(document, destination) != nil {
		return false
	}
	canonical, err := canonicalJSON(destination)
	return err == nil && bytes.Equal(document, canonical)
}

func encodeLeaseFrame(signer ed25519.PrivateKey, lease LeasePayload, credential []byte) ([]byte, error) {
	if len(signer) != ed25519.PrivateKeySize || !validCredentialBytes(credential) ||
		lease.CredentialByteLength != len(credential) {
		return nil, errors.New("credential broker lease response is invalid")
	}
	document, err := canonicalJSONLine(lease)
	if err != nil {
		return nil, err
	}
	// The digest and signature cover only public lease metadata. Credential bytes
	// travel exclusively as the disjoint binary frame payload.
	digest := sha256.Sum256(document)
	envelope := leaseEnvelope{
		ProtocolVersion: RemotePionProtocolVersion, Lease: lease,
		LeaseSHA256: hex.EncodeToString(digest[:]), SignatureAlgorithm: SignatureAlgorithm,
		Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, document)),
	}
	return encodeFrame(envelope, credential)
}

func encodeReceiptFrame(signer ed25519.PrivateKey, receipt ReceiptPayload) ([]byte, error) {
	if len(signer) != ed25519.PrivateKeySize {
		return nil, errors.New("credential broker receipt signer is invalid")
	}
	document, err := canonicalJSONLine(receipt)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(document)
	envelope := receiptEnvelope{
		ProtocolVersion: RemotePionProtocolVersion, Receipt: receipt,
		ReceiptSHA256: hex.EncodeToString(digest[:]), SignatureAlgorithm: SignatureAlgorithm,
		Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, document)),
	}
	return encodeFrame(envelope, nil)
}

func encodeFrame(metadata any, payload []byte) ([]byte, error) {
	metadataLine, err := canonicalJSONLine(metadata)
	if err != nil || len(metadataLine)+metadataLengthBytes+len(payload) > MaximumFrameBytes {
		return nil, errors.New("credential broker frame exceeded its authority")
	}
	frame := make([]byte, metadataLengthBytes+len(metadataLine)+len(payload))
	binary.BigEndian.PutUint32(frame[:metadataLengthBytes], uint32(len(metadataLine)))
	copy(frame[metadataLengthBytes:], metadataLine)
	copy(frame[metadataLengthBytes+len(metadataLine):], payload)
	return frame, nil
}

func readBoundedFrame(reader io.Reader) ([]byte, error) {
	frame, err := io.ReadAll(io.LimitReader(reader, MaximumFrameBytes+1))
	if err != nil || len(frame) == 0 || len(frame) > MaximumFrameBytes {
		erase(frame)
		return nil, ErrInvalidFrame
	}
	return frame, nil
}

func canonicalJSON(value any) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	document := output.Bytes()
	return append([]byte(nil), document[:len(document)-1]...), nil
}

func canonicalJSONLine(value any) ([]byte, error) {
	document, err := canonicalJSON(value)
	if err != nil {
		return nil, err
	}
	return append(document, '\n'), nil
}

func validCredentialBytes(value []byte) bool {
	if len(value) < minimumCredentialBytes || len(value) > maximumCredentialBytes {
		return false
	}
	for _, current := range value {
		alphaNumeric := current >= '0' && current <= '9' || current >= 'A' && current <= 'Z' ||
			current >= 'a' && current <= 'z'
		if !alphaNumeric && current != '-' && current != '_' {
			return false
		}
	}
	return true
}

func validOpaqueID(value string) bool { return opaqueIDPattern.MatchString(value) }

func validProfileID(value string) bool {
	return value == "scheduled-public-stun" || value == "scheduled-restricted-udp" ||
		value == "scheduled-coturn"
}

func parseTimestamp(value string) (time.Time, error) {
	parsed, err := time.Parse(canonicalTimestampLayout, value)
	if err != nil || parsed.Format(canonicalTimestampLayout) != value {
		return time.Time{}, errors.New("credential broker timestamp is invalid")
	}
	return parsed, nil
}

func erase(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
