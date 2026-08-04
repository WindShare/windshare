package main

import (
	"encoding/base64"
	"fmt"
	"net/netip"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/internal/testicetopology"
)

const identityBytes = 16

type pionCandidateEvidence struct {
	CandidateType string `json:"candidateType"`
	Protocol      string `json:"protocol"`
	Address       string `json:"address"`
	Port          uint16 `json:"port"`
	AddressFamily string `json:"addressFamily"`
}

type pionSelectedPairEvidence struct {
	Local  pionCandidateEvidence `json:"local"`
	Remote pionCandidateEvidence `json:"remote"`
}

func (runtime *topologyRuntime) selectedPairEvidence(
	peer *pion.PeerConnection,
) (pionSelectedPairEvidence, error) {
	if runtime == nil {
		return pionSelectedPairEvidence{}, fmt.Errorf("topology runtime is required")
	}
	if peer == nil || peer.SCTP() == nil || peer.SCTP().Transport() == nil ||
		peer.SCTP().Transport().ICETransport() == nil {
		return pionSelectedPairEvidence{}, fmt.Errorf("pion ICE transport is unavailable")
	}
	pair, err := peer.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil {
		return pionSelectedPairEvidence{}, fmt.Errorf("read Pion selected candidate pair: %w", err)
	}
	if pair == nil || pair.Local == nil || pair.Remote == nil {
		return pionSelectedPairEvidence{}, fmt.Errorf("pion selected candidate pair is unavailable")
	}
	evidence := pionSelectedPairEvidence{
		Local:  pionCandidateFromICE(pair.Local),
		Remote: pionCandidateFromICE(pair.Remote),
	}
	if err := runtime.validateSelectedPair(evidence); err != nil {
		return pionSelectedPairEvidence{}, err
	}
	return evidence, nil
}

func pionCandidateFromICE(candidate *pion.ICECandidate) pionCandidateEvidence {
	addressFamily := "unknown"
	if address, err := netip.ParseAddr(candidate.Address); err == nil {
		address = address.Unmap()
		switch {
		case address.Is4():
			addressFamily = testicetopology.AddressFamilyV4
		case address.Is6():
			addressFamily = "ipv6"
		}
	}
	return pionCandidateEvidence{
		CandidateType: candidate.Typ.String(),
		Protocol:      candidate.Protocol.String(),
		Address:       candidate.Address,
		Port:          candidate.Port,
		AddressFamily: addressFamily,
	}
}

func (runtime *topologyRuntime) validateSelectedPair(pair pionSelectedPairEvidence) error {
	if err := runtime.validateCandidate(pair.Local, true); err != nil {
		return fmt.Errorf("validate Pion local selected candidate: %w", err)
	}
	if err := runtime.validateCandidate(pair.Remote, false); err != nil {
		return fmt.Errorf("validate Pion remote selected candidate: %w", err)
	}
	if pair.Local.Address == pair.Remote.Address && pair.Local.Port == pair.Remote.Port &&
		pair.Local.Protocol == pair.Remote.Protocol {
		return fmt.Errorf("pion selected pair does not identify distinct transport endpoints")
	}
	return nil
}

func (runtime *topologyRuntime) validateCandidate(candidate pionCandidateEvidence, local bool) error {
	if candidate.AddressFamily != runtime.profile.AddressFamily ||
		!testicetopology.IsOperationalIPv4Unicast(candidate.Address) ||
		candidate.Port == 0 ||
		!contains(runtime.profile.CandidatePolicy.AllowedSelectedPairTypes, candidate.CandidateType) ||
		!contains(runtime.profile.CandidatePolicy.AllowedProtocols, candidate.Protocol) {
		return fmt.Errorf("candidate family, address, port, type, or protocol is outside the topology policy")
	}
	if local {
		if candidate.Address != runtime.resolution.Interface.SelectedAddress {
			return fmt.Errorf("local candidate is not bound to the resolved source address")
		}
		return nil
	}
	for _, eligible := range runtime.resolution.Interface.EligibleAddresses {
		if candidate.Address == eligible.Address {
			return nil
		}
	}
	return fmt.Errorf("remote candidate is absent from the resolved eligible address inventory")
}

func validAttemptID(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != identityBytes || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return false
	}
	for _, item := range decoded {
		if item != 0 {
			return true
		}
	}
	return false
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
