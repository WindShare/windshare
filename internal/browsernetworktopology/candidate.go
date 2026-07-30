package browsernetworktopology

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

var ErrInvalidCandidatePath = errors.New("invalid selected candidate path")

var candidateMDNSLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

type SelectedPairObservation string

const (
	SelectedPairPresent SelectedPairObservation = "present"
	SelectedPairAbsent  SelectedPairObservation = "absent"
)

type CandidatePath struct {
	SelectedPair        SelectedPairObservation `json:"selectedPair"`
	LocalCandidateType  *CandidateType          `json:"localCandidateType"`
	LocalAddress        *string                 `json:"localAddress"`
	LocalPort           *uint16                 `json:"localPort"`
	RemoteCandidateType *CandidateType          `json:"remoteCandidateType"`
	RemoteAddress       *string                 `json:"remoteAddress"`
	RemotePort          *uint16                 `json:"remotePort"`
	Protocol            *TransportProtocol      `json:"protocol"`
}

type CandidatePolicyOutcome string

const (
	CandidatePolicyMatched      CandidatePolicyOutcome = "matched"
	CandidatePolicyMismatched   CandidatePolicyOutcome = "mismatched"
	CandidatePolicyNotEvaluated CandidatePolicyOutcome = "not-evaluated"
)

type CandidateRationaleCode string

const (
	RationaleSelectedPairRequired               CandidateRationaleCode = "selected-pair-required"
	RationaleSelectedPairProhibited             CandidateRationaleCode = "selected-pair-prohibited"
	RationaleLocalCandidateTypeForbidden        CandidateRationaleCode = "local-candidate-type-forbidden"
	RationaleLocalCandidateTypeNotAllowed       CandidateRationaleCode = "local-candidate-type-not-allowed"
	RationaleLocalCandidateTypeRequiredMissing  CandidateRationaleCode = "local-candidate-type-required-missing"
	RationaleRemoteCandidateTypeForbidden       CandidateRationaleCode = "remote-candidate-type-forbidden"
	RationaleRemoteCandidateTypeNotAllowed      CandidateRationaleCode = "remote-candidate-type-not-allowed"
	RationaleRemoteCandidateTypeRequiredMissing CandidateRationaleCode = "remote-candidate-type-required-missing"
	RationaleProtocolForbidden                  CandidateRationaleCode = "protocol-forbidden"
	RationaleProtocolNotAllowed                 CandidateRationaleCode = "protocol-not-allowed"
	RationaleProtocolRequiredMissing            CandidateRationaleCode = "protocol-required-missing"
)

func (path CandidatePath) Validate() error {
	switch path.SelectedPair {
	case SelectedPairAbsent:
		if path.LocalCandidateType != nil || path.LocalAddress != nil || path.LocalPort != nil ||
			path.RemoteCandidateType != nil || path.RemoteAddress != nil || path.RemotePort != nil ||
			path.Protocol != nil {
			return fmt.Errorf("%w: absent selected pair carries candidate fields", ErrInvalidCandidatePath)
		}
	case SelectedPairPresent:
		if path.LocalCandidateType == nil || path.RemoteCandidateType == nil || path.Protocol == nil ||
			path.RemoteAddress == nil || path.RemotePort == nil || *path.RemotePort == 0 ||
			!validCandidateType(*path.LocalCandidateType) ||
			!validCandidateType(*path.RemoteCandidateType) || !validProtocol(*path.Protocol) ||
			!validCandidateLocalAddress(path.LocalAddress) ||
			(path.LocalPort != nil && *path.LocalPort == 0) || !validCandidateIPAddress(*path.RemoteAddress) {
			return fmt.Errorf("%w: present selected pair lacks a valid candidate type or protocol", ErrInvalidCandidatePath)
		}
	default:
		return fmt.Errorf("%w: selected-pair observation is unknown", ErrInvalidCandidatePath)
	}
	return nil
}

func validCandidateLocalAddress(address *string) bool {
	if address == nil {
		return true
	}
	if validCandidateIPAddress(*address) {
		return true
	}
	if len(*address) > 253 || !strings.HasSuffix(*address, ".local") {
		return false
	}
	for label := range strings.SplitSeq(*address, ".") {
		if len(label) < 1 || len(label) > 63 || !candidateMDNSLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

func validCandidateIPAddress(address string) bool {
	_, err := netip.ParseAddr(address)
	return err == nil
}

// EvaluateCandidatePath is browser-neutral: the same observed pair is classified
// identically regardless of which engine produced getStats evidence.
func EvaluateCandidatePath(
	policy CandidatePolicy,
	expectation ConnectivityExpectation,
	path CandidatePath,
) (CandidatePolicyOutcome, []CandidateRationaleCode, error) {
	if err := policy.Validate(expectation); err != nil {
		return "", nil, err
	}
	if err := path.Validate(); err != nil {
		return "", nil, err
	}

	if path.SelectedPair == SelectedPairAbsent {
		if policy.SelectedPair == SelectedPairRequired {
			return CandidatePolicyMismatched, []CandidateRationaleCode{RationaleSelectedPairRequired}, nil
		}
		return CandidatePolicyMatched, []CandidateRationaleCode{}, nil
	}
	if policy.SelectedPair == SelectedPairProhibited {
		return CandidatePolicyMismatched, []CandidateRationaleCode{RationaleSelectedPairProhibited}, nil
	}

	rationales := make([]CandidateRationaleCode, 0, 6)
	rationales = appendCandidateRationales(
		rationales,
		*path.LocalCandidateType,
		policy.LocalCandidateTypes,
		RationaleLocalCandidateTypeForbidden,
		RationaleLocalCandidateTypeNotAllowed,
		RationaleLocalCandidateTypeRequiredMissing,
	)
	rationales = appendCandidateRationales(
		rationales,
		*path.RemoteCandidateType,
		policy.RemoteCandidateTypes,
		RationaleRemoteCandidateTypeForbidden,
		RationaleRemoteCandidateTypeNotAllowed,
		RationaleRemoteCandidateTypeRequiredMissing,
	)
	rationales = appendProtocolRationales(rationales, *path.Protocol, policy.Protocols)
	if len(rationales) == 0 {
		return CandidatePolicyMatched, rationales, nil
	}
	return CandidatePolicyMismatched, rationales, nil
}

func appendCandidateRationales(
	rationales []CandidateRationaleCode,
	observed CandidateType,
	constraint CandidateTypeConstraint,
	forbiddenCode CandidateRationaleCode,
	notAllowedCode CandidateRationaleCode,
	requiredMissingCode CandidateRationaleCode,
) []CandidateRationaleCode {
	if containsCandidateType(constraint.Forbidden, observed) {
		rationales = append(rationales, forbiddenCode)
	} else if !containsCandidateType(constraint.Allowed, observed) {
		rationales = append(rationales, notAllowedCode)
	}
	if len(constraint.Required) == 1 && constraint.Required[0] != observed {
		rationales = append(rationales, requiredMissingCode)
	}
	return rationales
}

func appendProtocolRationales(
	rationales []CandidateRationaleCode,
	observed TransportProtocol,
	constraint ProtocolConstraint,
) []CandidateRationaleCode {
	if containsProtocol(constraint.Forbidden, observed) {
		rationales = append(rationales, RationaleProtocolForbidden)
	} else if !containsProtocol(constraint.Allowed, observed) {
		rationales = append(rationales, RationaleProtocolNotAllowed)
	}
	if len(constraint.Required) == 1 && constraint.Required[0] != observed {
		rationales = append(rationales, RationaleProtocolRequiredMissing)
	}
	return rationales
}

func exactRationaleCodes(actual, expected []CandidateRationaleCode) bool {
	if len(actual) != len(expected) {
		return false
	}
	for index := range expected {
		if actual[index] != expected[index] {
			return false
		}
	}
	return true
}
