package turncontrol

import (
	"bytes"
	"time"
	"unicode/utf8"

	"github.com/fxamacker/cbor/v2"
)

const (
	demandKind = uint64(iota + 1)
	grantKind
	readyKind
	retireKind
	unavailableKind
	availableKind
	reportKind
)

var encoder, _ = cbor.CoreDetEncOptions().EncMode()
var decoder, _ = (cbor.DecOptions{
	MaxArrayElements: 32, MaxMapPairs: 16, MaxNestedLevels: 4,
	IndefLength: cbor.IndefLengthForbidden, TagsMd: cbor.TagsForbidden,
	DupMapKey: cbor.DupMapKeyEnforcedAPF,
}).DecMode()

func Encode(message Message) ([]byte, error) {
	if message == nil {
		return nil, ErrMessage
	}
	h := message.Header()
	if h.SessionID == ([IdentityBytes]byte{}) || h.OwnerPathID == ([IdentityBytes]byte{}) || h.Sequence == 0 {
		return nil, ErrMessage
	}
	base := func(kind uint64) []any { return []any{Schema, kind, h.SessionID[:], h.OwnerPathID[:], h.Sequence} }
	var fields []any
	switch value := message.(type) {
	case Demand:
		if !duration(value.HoldFor, MaximumDemandLifetime) {
			return nil, ErrMessage
		}
		fields = append(base(demandKind), uint64(value.HoldFor/time.Millisecond))
	case Grant:
		if !validGrant(value) {
			return nil, ErrMessage
		}
		fields = append(base(grantKind), value.PathID[:], value.Replaces[:], value.Provider, value.Account, value.Profile,
			uint64(value.CredentialKind), value.Username, value.Password, uint64(value.ValidFor/time.Millisecond), value.URLs)
	case Ready:
		if value.PathID == ([IdentityBytes]byte{}) {
			return nil, ErrMessage
		}
		fields = append(base(readyKind), value.PathID[:])
	case Report:
		if value.PathID == ([IdentityBytes]byte{}) {
			return nil, ErrMessage
		}
		fields = append(base(reportKind), value.PathID[:], value.UsefulBytes)
	case Retire:
		if value.PathID == ([IdentityBytes]byte{}) || value.Reason < Idle || value.Reason > Unhelpful {
			return nil, ErrMessage
		}
		fields = append(base(retireKind), value.PathID[:], uint64(value.Reason))
	case Unavailable:
		if !identifier(value.Profile) || !text(value.Reason, MaximumTextBytes) ||
			!duration(value.RetryAfter, MaximumDemandLifetime) {
			return nil, ErrMessage
		}
		fields = append(base(unavailableKind), value.Profile, uint64(value.RetryAfter/time.Millisecond), value.Reason)
	case Available:
		if len(value.Profiles) == 0 || len(value.Profiles) > MaximumURLs {
			return nil, ErrMessage
		}
		for _, profile := range value.Profiles {
			if !identifier(profile) {
				return nil, ErrMessage
			}
		}
		fields = append(base(availableKind), value.Profiles)
	default:
		return nil, ErrMessage
	}
	body, err := encoder.Marshal(fields)
	if err != nil || len(body) > MaximumBodyBytes {
		return nil, ErrMessage
	}
	return body, nil
}

func validGrant(value Grant) bool {
	if value.PathID == ([IdentityBytes]byte{}) || value.PathID == value.Replaces ||
		!identifier(value.Provider) || !identifier(value.Account) || !identifier(value.Profile) ||
		!text(value.Username, MaximumTextBytes) || !text(value.Password, MaximumTextBytes) ||
		value.CredentialKind < Fixed || value.CredentialKind > Expiring ||
		!duration(value.ValidFor, MaximumCredentialLifetime) ||
		(value.CredentialKind == Fixed) != (value.ValidFor == 0) ||
		len(value.URLs) == 0 || len(value.URLs) > MaximumURLs {
		return false
	}
	for _, raw := range value.URLs {
		if !text(raw, MaximumURLBytes) {
			return false
		}
	}
	return true
}

func Decode(body []byte) (Message, error) {
	if len(body) > MaximumBodyBytes {
		return nil, ErrMessage
	}
	var fields []cbor.RawMessage
	if decoder.Unmarshal(body, &fields) != nil || len(fields) < 6 {
		return nil, ErrMessage
	}
	var schema, kind uint64
	var h Header
	if decode(fields[:5], &schema, &kind, &h.SessionID, &h.OwnerPathID, &h.Sequence) != nil || schema != Schema {
		return nil, ErrMessage
	}
	var message Message
	switch kind {
	case demandKind:
		var hold uint64
		if decode(fields[5:], &hold) != nil || hold > uint64(MaximumDemandLifetime/time.Millisecond) {
			return nil, ErrMessage
		}
		message = Demand{h, time.Duration(hold) * time.Millisecond}
	case grantKind:
		value := Grant{Authority: h}
		var lifetime, credentialKind uint64
		if decode(fields[5:], &value.PathID, &value.Replaces, &value.Provider, &value.Account, &value.Profile,
			&credentialKind, &value.Username, &value.Password, &lifetime, &value.URLs) != nil ||
			lifetime > uint64(MaximumCredentialLifetime/time.Millisecond) || credentialKind > uint64(Expiring) {
			return nil, ErrMessage
		}
		value.ValidFor = time.Duration(lifetime) * time.Millisecond
		value.CredentialKind = CredentialKind(credentialKind)
		message = value
	case readyKind:
		value := Ready{Authority: h}
		if decode(fields[5:], &value.PathID) != nil {
			return nil, ErrMessage
		}
		message = value
	case reportKind:
		value := Report{Authority: h}
		if decode(fields[5:], &value.PathID, &value.UsefulBytes) != nil {
			return nil, ErrMessage
		}
		message = value
	case retireKind:
		value := Retire{Authority: h}
		var reason uint64
		if decode(fields[5:], &value.PathID, &reason) != nil || reason > uint64(Unhelpful) {
			return nil, ErrMessage
		}
		value.Reason = RetireReason(reason)
		message = value
	case unavailableKind:
		value := Unavailable{Authority: h}
		var retry uint64
		if decode(fields[5:], &value.Profile, &retry, &value.Reason) != nil ||
			retry > uint64(MaximumDemandLifetime/time.Millisecond) {
			return nil, ErrMessage
		}
		value.RetryAfter = time.Duration(retry) * time.Millisecond
		message = value
	case availableKind:
		value := Available{Authority: h}
		if decode(fields[5:], &value.Profiles) != nil {
			return nil, ErrMessage
		}
		message = value
	default:
		return nil, ErrMessage
	}
	canonical, err := Encode(message)
	if err != nil || !bytes.Equal(body, canonical) {
		return nil, ErrMessage
	}
	return message, nil
}

// IsMessage inspects only the canonical envelope discriminator. Full validation
// is still required before delivery to an authenticated control consumer.
func IsMessage(body []byte) bool {
	return len(body) > 2 && body[0] >= 0x86 && body[0] <= 0x8f && body[1] == byte(Schema)
}
func decode(fields []cbor.RawMessage, targets ...any) error {
	if len(fields) != len(targets) {
		return ErrMessage
	}
	for i, target := range targets {
		if decoder.Unmarshal(fields[i], target) != nil {
			return ErrMessage
		}
	}
	return nil
}
func duration(value, maximum time.Duration) bool {
	return value >= 0 && value <= maximum && value%time.Millisecond == 0
}
func text(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && utf8.ValidString(value)
}
func identifier(value string) bool {
	if !text(value, MaximumIdentifierBytes) {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.' {
			continue
		}
		return false
	}
	return true
}
