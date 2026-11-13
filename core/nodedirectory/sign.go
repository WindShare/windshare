package nodedirectory

import (
	"crypto/ed25519"
	"encoding/json"
)

func Certify(certificate Certificate, rootID string, root ed25519.PrivateKey) (SignedCertificate, error) {
	if len(root) != ed25519.PrivateKeySize || !validText(rootID) {
		return SignedCertificate{}, ErrFormat
	}
	payload, err := json.Marshal(certificate)
	if err != nil {
		return SignedCertificate{}, err
	}
	return SignedCertificate{Root: rootID, Payload: encoding.EncodeToString(payload),
		Signature: encoding.EncodeToString(ed25519.Sign(root, append([]byte(CertificateDomain), payload...)))}, nil
}

// Sign verifies the complete chain before exporting it, catching mismatched keys
// and generations locally rather than publishing an unusable directory.
func Sign(payload Payload, certificate SignedCertificate, key ed25519.PrivateKey, roots []Root) ([]byte, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, ErrFormat
	}
	if err := Validate(payload); err != nil {
		return nil, err
	}
	bytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	result, err := json.Marshal(Envelope{Payload: encoding.EncodeToString(bytes), Certificate: certificate,
		Signature: encoding.EncodeToString(ed25519.Sign(key, append([]byte(PayloadDomain), bytes...)))})
	if err != nil {
		return nil, err
	}
	if _, err := Verify(result, payload.Network, roots); err != nil {
		return nil, err
	}
	return result, nil
}
