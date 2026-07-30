// Package testicetopology owns the serialized, test-only ICE topology shared
// by browser fixtures and Pion processes. Production peer construction must not
// import this package or accept its profile or resolution paths.
package testicetopology

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	ProfileSchemaVersion    = 1
	ResolutionSchemaVersion = 1
	PRTopologyID            = "pr-same-host-kernel-route-ipv4"
	SourceSelectorAlgorithm = "udp-connect-source-consensus-v1"
	AddressFamilyV4         = "ipv4"
	TransportAll            = "all"
	CandidateHost           = "host"
	CandidatePRFLX          = "prflx"
	ProtocolUDP             = "udp"
	MaximumFileBytes        = 16 << 10

	maximumInterfaceNameBytes = 255
	maximumSHA256TextBytes    = 64
)

var (
	ErrInvalidProfile    = errors.New("test ICE topology profile is invalid")
	ErrInvalidResolution = errors.New("test ICE topology resolution is invalid")
)

type ProbeDestination struct {
	Address string `json:"address"`
	Port    uint16 `json:"port"`
}

type SourceSelector struct {
	Algorithm         string             `json:"algorithm"`
	ProbeDestinations []ProbeDestination `json:"probeDestinations"`
}

type RTCConfiguration struct {
	ICEServers         []struct{} `json:"iceServers"`
	ICETransportPolicy string     `json:"iceTransportPolicy"`
}

type CandidatePolicy struct {
	AllowedSelectedPairTypes []string `json:"allowedSelectedPairTypes"`
	AllowedProtocols         []string `json:"allowedProtocols"`
}

type Profile struct {
	TopologyProfileSchemaVersion int              `json:"topologyProfileSchemaVersion"`
	TopologyID                   string           `json:"topologyId"`
	SourceSelector               SourceSelector   `json:"sourceSelector"`
	AddressFamily                string           `json:"addressFamily"`
	RTCConfiguration             RTCConfiguration `json:"rtcConfiguration"`
	CandidatePolicy              CandidatePolicy  `json:"candidatePolicy"`
}

type ProbeResult struct {
	DestinationAddress string `json:"destinationAddress"`
	DestinationPort    uint16 `json:"destinationPort"`
	SourceAddress      string `json:"sourceAddress"`
}

type EligibleAddress struct {
	Address      string `json:"address"`
	PrefixLength uint8  `json:"prefixLength"`
}

type ResolvedInterface struct {
	Index             uint32            `json:"index"`
	Name              string            `json:"name"`
	SelectedAddress   string            `json:"selectedAddress"`
	EligibleAddresses []EligibleAddress `json:"eligibleAddresses"`
}

type Resolution struct {
	TopologyResolutionSchemaVersion int               `json:"topologyResolutionSchemaVersion"`
	TopologyID                      string            `json:"topologyId"`
	TopologyProfileSHA256           string            `json:"topologyProfileSha256"`
	SelectorAlgorithm               string            `json:"selectorAlgorithm"`
	AddressFamily                   string            `json:"addressFamily"`
	ProbeResults                    []ProbeResult     `json:"probeResults"`
	Interface                       ResolvedInterface `json:"interface"`
}

func FrozenProbeDestinations() []ProbeDestination {
	return []ProbeDestination{
		{Address: "192.0.2.1", Port: 9},
		{Address: "198.51.100.1", Port: 9},
		{Address: "203.0.113.1", Port: 9},
	}
}

func Load(path string) (Profile, error) {
	encoded, err := readBoundedFile(path, "test ICE topology", ErrInvalidProfile)
	if err != nil {
		return Profile{}, err
	}
	return Parse(encoded)
}

func Parse(encoded []byte) (Profile, error) {
	if err := validateCanonicalJSON(encoded, "test ICE topology"); err != nil {
		return Profile{}, fmt.Errorf("%w: %v", ErrInvalidProfile, err)
	}
	if err := requireExactProfileShape(encoded); err != nil {
		return Profile{}, fmt.Errorf("%w: %v", ErrInvalidProfile, err)
	}
	var profile Profile
	if err := decodeCanonicalJSON(encoded, "test ICE topology", &profile); err != nil {
		return Profile{}, fmt.Errorf("%w: %v", ErrInvalidProfile, err)
	}
	if err := profile.Validate(); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

func (profile Profile) Validate() error {
	valid := profile.TopologyProfileSchemaVersion == ProfileSchemaVersion &&
		profile.TopologyID == PRTopologyID &&
		profile.SourceSelector.Algorithm == SourceSelectorAlgorithm &&
		exactProbeDestinations(profile.SourceSelector.ProbeDestinations) &&
		profile.AddressFamily == AddressFamilyV4 &&
		profile.RTCConfiguration.ICEServers != nil &&
		len(profile.RTCConfiguration.ICEServers) == 0 &&
		profile.RTCConfiguration.ICETransportPolicy == TransportAll &&
		exactStrings(profile.CandidatePolicy.AllowedSelectedPairTypes, CandidateHost, CandidatePRFLX) &&
		exactStrings(profile.CandidatePolicy.AllowedProtocols, ProtocolUDP)
	if !valid {
		return fmt.Errorf("%w: unsupported version, topology, selector, family, RTC configuration, or candidate policy", ErrInvalidProfile)
	}
	return nil
}

func LoadResolution(path string, profile Profile, expectedProfileSHA256 string) (Resolution, error) {
	encoded, err := readBoundedFile(path, "test ICE topology resolution", ErrInvalidResolution)
	if err != nil {
		return Resolution{}, err
	}
	return ParseResolution(encoded, profile, expectedProfileSHA256)
}

func ParseResolution(
	encoded []byte,
	profile Profile,
	expectedProfileSHA256 string,
) (Resolution, error) {
	if err := profile.Validate(); err != nil {
		return Resolution{}, errors.Join(ErrInvalidResolution, err)
	}
	if !isSHA256(expectedProfileSHA256) {
		return Resolution{}, fmt.Errorf("%w: expected profile SHA-256 is not canonical", ErrInvalidResolution)
	}
	if err := validateCanonicalJSON(encoded, "test ICE topology resolution"); err != nil {
		return Resolution{}, fmt.Errorf("%w: %v", ErrInvalidResolution, err)
	}
	if err := requireExactResolutionShape(encoded); err != nil {
		return Resolution{}, fmt.Errorf("%w: %v", ErrInvalidResolution, err)
	}
	var resolution Resolution
	if err := decodeCanonicalJSON(encoded, "test ICE topology resolution", &resolution); err != nil {
		return Resolution{}, fmt.Errorf("%w: %v", ErrInvalidResolution, err)
	}
	if err := resolution.Validate(profile, expectedProfileSHA256); err != nil {
		return Resolution{}, err
	}
	return resolution, nil
}

func (resolution Resolution) Validate(profile Profile, expectedProfileSHA256 string) error {
	if err := profile.Validate(); err != nil {
		return errors.Join(ErrInvalidResolution, err)
	}
	canonicalProfileSHA256, err := profile.SHA256()
	if err != nil {
		return errors.Join(ErrInvalidResolution, err)
	}
	if !isSHA256(expectedProfileSHA256) ||
		expectedProfileSHA256 != canonicalProfileSHA256 ||
		resolution.TopologyResolutionSchemaVersion != ResolutionSchemaVersion ||
		resolution.TopologyID != profile.TopologyID ||
		resolution.TopologyProfileSHA256 != expectedProfileSHA256 ||
		resolution.SelectorAlgorithm != profile.SourceSelector.Algorithm ||
		resolution.AddressFamily != profile.AddressFamily ||
		len(resolution.ProbeResults) != len(profile.SourceSelector.ProbeDestinations) {
		return fmt.Errorf("%w: schema, profile binding, selector, family, or probe registry differs", ErrInvalidResolution)
	}
	for index, result := range resolution.ProbeResults {
		expected := profile.SourceSelector.ProbeDestinations[index]
		if result.DestinationAddress != expected.Address ||
			result.DestinationPort != expected.Port ||
			!IsOperationalIPv4Unicast(result.SourceAddress) ||
			result.SourceAddress != resolution.Interface.SelectedAddress {
			return fmt.Errorf("%w: probe result %d differs from the frozen unanimous source selection", ErrInvalidResolution, index)
		}
	}
	if resolution.Interface.Index == 0 ||
		!validNFCText(resolution.Interface.Name, maximumInterfaceNameBytes) ||
		!IsOperationalIPv4Unicast(resolution.Interface.SelectedAddress) ||
		len(resolution.Interface.EligibleAddresses) == 0 {
		return fmt.Errorf("%w: resolved interface identity or selected address is invalid", ErrInvalidResolution)
	}
	seen := make(map[string]struct{}, len(resolution.Interface.EligibleAddresses))
	selectedPresent := false
	var previousAddress uint32
	var previousPrefix uint8
	for index, candidate := range resolution.Interface.EligibleAddresses {
		numeric, valid := ipv4Number(candidate.Address)
		if !valid || !IsOperationalIPv4Unicast(candidate.Address) ||
			candidate.PrefixLength < 1 || candidate.PrefixLength > 32 {
			return fmt.Errorf("%w: eligible interface address %d is invalid", ErrInvalidResolution, index)
		}
		if _, duplicate := seen[candidate.Address]; duplicate {
			return fmt.Errorf("%w: eligible interface address inventory contains duplicates", ErrInvalidResolution)
		}
		seen[candidate.Address] = struct{}{}
		if index > 0 && (numeric < previousAddress ||
			(numeric == previousAddress && candidate.PrefixLength < previousPrefix)) {
			return fmt.Errorf("%w: eligible interface address inventory is not canonically ordered", ErrInvalidResolution)
		}
		previousAddress = numeric
		previousPrefix = candidate.PrefixLength
		selectedPresent = selectedPresent || candidate.Address == resolution.Interface.SelectedAddress
	}
	if !selectedPresent {
		return fmt.Errorf("%w: selected address is absent from the eligible interface inventory", ErrInvalidResolution)
	}
	return nil
}

func (profile Profile) CanonicalJSON() ([]byte, error) {
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	var writer canonicalJSONWriter
	writer.WriteString(`{"topologyProfileSchemaVersion":`)
	writer.WriteString(strconv.Itoa(profile.TopologyProfileSchemaVersion))
	writer.WriteString(`,"topologyId":`)
	writer.string(profile.TopologyID)
	writer.WriteString(`,"sourceSelector":{"algorithm":`)
	writer.string(profile.SourceSelector.Algorithm)
	writer.WriteString(`,"probeDestinations":[`)
	for index, probe := range profile.SourceSelector.ProbeDestinations {
		if index > 0 {
			writer.WriteByte(',')
		}
		writer.WriteString(`{"address":`)
		writer.string(probe.Address)
		writer.WriteString(`,"port":`)
		writer.WriteString(strconv.FormatUint(uint64(probe.Port), 10))
		writer.WriteByte('}')
	}
	writer.WriteString(`]},"addressFamily":`)
	writer.string(profile.AddressFamily)
	writer.WriteString(`,"rtcConfiguration":{"iceServers":[],"iceTransportPolicy":`)
	writer.string(profile.RTCConfiguration.ICETransportPolicy)
	writer.WriteString(`},"candidatePolicy":{"allowedSelectedPairTypes":[`)
	writeStringArray(&writer, profile.CandidatePolicy.AllowedSelectedPairTypes)
	writer.WriteString(`],"allowedProtocols":[`)
	writeStringArray(&writer, profile.CandidatePolicy.AllowedProtocols)
	writer.WriteString(`]}}`)
	return bytes.Clone(writer.Bytes()), nil
}

func (profile Profile) SHA256() (string, error) {
	encoded, err := profile.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return sha256Text(encoded), nil
}

func (resolution Resolution) CanonicalJSON(
	profile Profile,
	expectedProfileSHA256 string,
) ([]byte, error) {
	if err := resolution.Validate(profile, expectedProfileSHA256); err != nil {
		return nil, err
	}
	var writer canonicalJSONWriter
	writer.WriteString(`{"topologyResolutionSchemaVersion":`)
	writer.WriteString(strconv.Itoa(resolution.TopologyResolutionSchemaVersion))
	writer.WriteString(`,"topologyId":`)
	writer.string(resolution.TopologyID)
	writer.WriteString(`,"topologyProfileSha256":`)
	writer.string(resolution.TopologyProfileSHA256)
	writer.WriteString(`,"selectorAlgorithm":`)
	writer.string(resolution.SelectorAlgorithm)
	writer.WriteString(`,"addressFamily":`)
	writer.string(resolution.AddressFamily)
	writer.WriteString(`,"probeResults":[`)
	for index, result := range resolution.ProbeResults {
		if index > 0 {
			writer.WriteByte(',')
		}
		writer.WriteString(`{"destinationAddress":`)
		writer.string(result.DestinationAddress)
		writer.WriteString(`,"destinationPort":`)
		writer.WriteString(strconv.FormatUint(uint64(result.DestinationPort), 10))
		writer.WriteString(`,"sourceAddress":`)
		writer.string(result.SourceAddress)
		writer.WriteByte('}')
	}
	writer.WriteString(`],"interface":{"index":`)
	writer.WriteString(strconv.FormatUint(uint64(resolution.Interface.Index), 10))
	writer.WriteString(`,"name":`)
	writer.string(resolution.Interface.Name)
	writer.WriteString(`,"selectedAddress":`)
	writer.string(resolution.Interface.SelectedAddress)
	writer.WriteString(`,"eligibleAddresses":[`)
	for index, candidate := range resolution.Interface.EligibleAddresses {
		if index > 0 {
			writer.WriteByte(',')
		}
		writer.WriteString(`{"address":`)
		writer.string(candidate.Address)
		writer.WriteString(`,"prefixLength":`)
		writer.WriteString(strconv.FormatUint(uint64(candidate.PrefixLength), 10))
		writer.WriteByte('}')
	}
	writer.WriteString(`]}}`)
	return bytes.Clone(writer.Bytes()), nil
}

func (resolution Resolution) SHA256(profile Profile, expectedProfileSHA256 string) (string, error) {
	encoded, err := resolution.CanonicalJSON(profile, expectedProfileSHA256)
	if err != nil {
		return "", err
	}
	return sha256Text(encoded), nil
}

func IsOperationalIPv4Unicast(address string) bool {
	octets := strings.Split(address, ".")
	if len(octets) != 4 {
		return false
	}
	values := [4]uint8{}
	for index, octet := range octets {
		if octet == "" || len(octet) > 3 || (len(octet) > 1 && octet[0] == '0') {
			return false
		}
		value := 0
		for _, digit := range octet {
			if digit < '0' || digit > '9' {
				return false
			}
			value = value*10 + int(digit-'0')
		}
		if value > 255 {
			return false
		}
		values[index] = uint8(value)
	}
	return values[0] != 0 && values[0] != 127 && values[0] < 224 &&
		!(values[0] == 169 && values[1] == 254)
}

func ipv4Number(address string) (uint32, bool) {
	if !IsOperationalIPv4Unicast(address) {
		return 0, false
	}
	var result uint32
	for octet := range strings.SplitSeq(address, ".") {
		value, _ := strconv.ParseUint(octet, 10, 8)
		result = result<<8 | uint32(value)
	}
	return result, true
}

func readBoundedFile(path, label string, sentinel error) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", label, err)
	}
	defer file.Close()
	encoded, err := io.ReadAll(io.LimitReader(file, MaximumFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", label, err)
	}
	if len(encoded) > MaximumFileBytes {
		return nil, fmt.Errorf("%w: %s exceeds %d bytes", sentinel, label, MaximumFileBytes)
	}
	return encoded, nil
}

func requireExactProfileShape(encoded []byte) error {
	root, err := rawObject(encoded, "test ICE topology")
	if err != nil {
		return err
	}
	if err := requireExactKeys(root, "test ICE topology",
		"topologyProfileSchemaVersion", "topologyId", "sourceSelector",
		"addressFamily", "rtcConfiguration", "candidatePolicy"); err != nil {
		return err
	}
	selector, err := rawObject(root["sourceSelector"], "test ICE source selector")
	if err != nil {
		return err
	}
	if err := requireExactKeys(selector, "test ICE source selector", "algorithm", "probeDestinations"); err != nil {
		return err
	}
	var probes []json.RawMessage
	if err := json.Unmarshal(selector["probeDestinations"], &probes); err != nil || probes == nil {
		return fmt.Errorf("test ICE source selector probes must be an array")
	}
	for index, encodedProbe := range probes {
		probe, probeErr := rawObject(encodedProbe, fmt.Sprintf("test ICE source selector probe %d", index))
		if probeErr != nil {
			return probeErr
		}
		if keyErr := requireExactKeys(probe, fmt.Sprintf("test ICE source selector probe %d", index), "address", "port"); keyErr != nil {
			return keyErr
		}
	}
	rtc, err := rawObject(root["rtcConfiguration"], "test ICE RTC configuration")
	if err != nil {
		return err
	}
	if err := requireExactKeys(rtc, "test ICE RTC configuration", "iceServers", "iceTransportPolicy"); err != nil {
		return err
	}
	policy, err := rawObject(root["candidatePolicy"], "test ICE candidate policy")
	if err != nil {
		return err
	}
	return requireExactKeys(policy, "test ICE candidate policy", "allowedSelectedPairTypes", "allowedProtocols")
}

func requireExactResolutionShape(encoded []byte) error {
	root, err := rawObject(encoded, "test ICE topology resolution")
	if err != nil {
		return err
	}
	if err := requireExactKeys(root, "test ICE topology resolution",
		"topologyResolutionSchemaVersion", "topologyId", "topologyProfileSha256",
		"selectorAlgorithm", "addressFamily", "probeResults", "interface"); err != nil {
		return err
	}
	var probes []json.RawMessage
	if err := json.Unmarshal(root["probeResults"], &probes); err != nil || probes == nil {
		return fmt.Errorf("test ICE probe results must be an array")
	}
	for index, encodedProbe := range probes {
		probe, probeErr := rawObject(encodedProbe, fmt.Sprintf("test ICE probe result %d", index))
		if probeErr != nil {
			return probeErr
		}
		if keyErr := requireExactKeys(probe, fmt.Sprintf("test ICE probe result %d", index),
			"destinationAddress", "destinationPort", "sourceAddress"); keyErr != nil {
			return keyErr
		}
	}
	resolved, err := rawObject(root["interface"], "test ICE resolved interface")
	if err != nil {
		return err
	}
	if err := requireExactKeys(resolved, "test ICE resolved interface",
		"index", "name", "selectedAddress", "eligibleAddresses"); err != nil {
		return err
	}
	var addresses []json.RawMessage
	if err := json.Unmarshal(resolved["eligibleAddresses"], &addresses); err != nil || addresses == nil {
		return fmt.Errorf("test ICE eligible addresses must be an array")
	}
	for index, encodedAddress := range addresses {
		address, addressErr := rawObject(encodedAddress, fmt.Sprintf("test ICE eligible address %d", index))
		if addressErr != nil {
			return addressErr
		}
		if keyErr := requireExactKeys(address, fmt.Sprintf("test ICE eligible address %d", index),
			"address", "prefixLength"); keyErr != nil {
			return keyErr
		}
	}
	return nil
}

func rawObject(encoded []byte, label string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil || object == nil {
		return nil, fmt.Errorf("%s must be an object", label)
	}
	return object, nil
}

func requireExactKeys(object map[string]json.RawMessage, label string, fields ...string) error {
	if len(object) != len(fields) {
		return fmt.Errorf("%s fields differ from the frozen contract", label)
	}
	for _, field := range fields {
		if _, present := object[field]; !present {
			return fmt.Errorf("%s is missing exact field %q", label, field)
		}
	}
	return nil
}

func exactProbeDestinations(actual []ProbeDestination) bool {
	expected := FrozenProbeDestinations()
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

func exactStrings(actual []string, expected ...string) bool {
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

func validNFCText(value string, maximumBytes int) bool {
	return value != "" && utf8.ValidString(value) &&
		!strings.ContainsRune(value, utf8.RuneError) &&
		len(value) <= maximumBytes && norm.NFC.IsNormalString(value)
}

func isSHA256(value string) bool {
	if len(value) != maximumSHA256TextBytes {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func writeStringArray(writer *canonicalJSONWriter, values []string) {
	for index, value := range values {
		if index > 0 {
			writer.WriteByte(',')
		}
		writer.string(value)
	}
}

func sha256Text(encoded []byte) string {
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
