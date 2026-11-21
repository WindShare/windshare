package serviceauthority

import "time"

type Entitlement struct {
	Version        uint64 `json:"version"`
	Day            uint64 `json:"day"`
	ValidUntil     uint64 `json:"valid_until"`
	RemainingBytes uint64 `json:"remaining_bytes"`
}

type Allowance string

const (
	AllowanceAvailable Allowance = "available"
	AllowanceSpent     Allowance = "spent"
	AllowanceStale     Allowance = "stale"
)

func (value Entitlement) Allowance(now time.Time) Allowance {
	if now.Unix() < 0 || uint64(now.Unix())/DaySeconds != value.Day || uint64(now.Unix()) >= value.ValidUntil {
		return AllowanceStale
	}
	if value.RemainingBytes == 0 {
		return AllowanceSpent
	}
	return AllowanceAvailable
}

func (value Entitlement) ValidSnapshot(issued uint64) bool {
	return value.Version > 0 && issued <= MaximumCounter && value.Day == issued/DaySeconds &&
		value.ValidUntil > issued && value.ValidUntil <= issued+EntitlementLifetime &&
		value.ValidUntil <= (value.Day+1)*DaySeconds
}

func (value Entitlement) supersedes(previous Entitlement) bool {
	if value.Day != previous.Day {
		return value.Day > previous.Day
	}
	if value.Version != previous.Version {
		return value.Version > previous.Version
	}
	return value.ValidUntil > previous.ValidUntil
}

func Advance(previous, next Entitlement) Entitlement {
	if next.supersedes(previous) {
		return next
	}
	return previous
}

func (value Entitlement) ResetAt() time.Time {
	if value.Version == 0 {
		return time.Time{}
	}
	return time.Unix(int64((value.Day+1)*DaySeconds), 0).UTC()
}

type Ticket struct {
	Issuer      [32]byte
	KeyID       uint64
	Account     [16]byte
	Credential  [16]byte
	IssuedAt    uint64
	ExpiresAt   uint64
	Entitlement Entitlement
}
type entitlementWire struct {
	_              struct{} `cbor:",toarray"`
	Version        uint64
	Day            uint64
	ValidUntil     uint64
	RemainingBytes uint64
}
type ticketWire struct {
	_           struct{} `cbor:",toarray"`
	Format      uint64
	Issuer      []byte
	KeyID       uint64
	Account     []byte
	Credential  []byte
	IssuedAt    uint64
	ExpiresAt   uint64
	Audience    string
	Entitlement entitlementWire
}

func VerifyTicket(encoded string, generation Generation, now time.Time) (Ticket, error) {
	payload, err := VerifySigned(TicketDomain, encoded, generation.Key, MaximumTicketBytes)
	if err != nil {
		return Ticket{}, err
	}
	var wire ticketWire
	if decodeCanonical(payload, &wire) != nil || wire.Format != 1 || wire.Audience != "official" ||
		len(wire.Issuer) != 32 || len(wire.Account) != 16 || len(wire.Credential) != 16 {
		return Ticket{}, ErrFormat
	}
	snapshot := wire.Entitlement
	ticket := Ticket{Issuer: [32]byte(wire.Issuer), KeyID: wire.KeyID, Account: [16]byte(wire.Account),
		Credential: [16]byte(wire.Credential), IssuedAt: wire.IssuedAt, ExpiresAt: wire.ExpiresAt,
		Entitlement: Entitlement{snapshot.Version, snapshot.Day, snapshot.ValidUntil, snapshot.RemainingBytes}}
	if ticket.Issuer != generation.Issuer || ticket.KeyID != generation.Number {
		return Ticket{}, ErrRollback
	}
	if ticket.Account == [16]byte{} || ticket.Credential == [16]byte{} ||
		ticket.ExpiresAt <= ticket.IssuedAt || ticket.ExpiresAt-ticket.IssuedAt > TicketLifetime ||
		!ticket.Entitlement.ValidSnapshot(ticket.IssuedAt) {
		return Ticket{}, ErrFormat
	}
	if !generation.Usable(now) || uint64(now.Unix())+ClockSkew < ticket.IssuedAt ||
		uint64(now.Unix()) >= ticket.ExpiresAt || ticket.IssuedAt < generation.NotBefore ||
		ticket.ExpiresAt > generation.ExpiresAt {
		return Ticket{}, ErrExpired
	}
	return ticket, nil
}
