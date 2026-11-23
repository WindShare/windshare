package protocolsession

import (
	"bytes"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/windshare/windshare/core/networkendpoint"
)

const (
	MaxGrantsPerMessage    = 4
	MaxURLsPerGrant        = 8
	MaxTURNURLBytes        = 512
	MaxTURNCredentialBytes = 1024
	MaxTURNBodyBytes       = 16 << 10
	MaxTURNServiceIDBytes  = 128
	turnSchema             = 1
	turnDemandKind         = 1
	turnGrantsKind         = 2
)

type TURNMode uint8

const (
	TURNParallel TURNMode = iota + 1
	TURNAll
)

type TURNStatus uint8

const (
	TURNOK TURNStatus = iota + 1
	TURNUnconfigured
	TURNRetryLater
)

var ErrTURNControl = errors.New("invalid TURN control")
var turnURL = regexp.MustCompile(`^turns?:([a-zA-Z0-9._-]+|\[[a-fA-F0-9:.]+\])(?::([0-9]+))?(?:\?transport=(?:udp|tcp))?$`)

func ValidTURNURL(value string) bool {
	if len(value) > MaxTURNURLBytes {
		return false
	}
	match := turnURL.FindStringSubmatch(value)
	if match == nil || !networkendpoint.ValidICEHost(match[1]) {
		return false
	}
	if match[2] == "" {
		return true
	}
	port, err := strconv.ParseUint(match[2], 10, 16)
	return err == nil && port != 0
}

type TURNGrantID [IdentityBytes]byte
type TURNGrant struct {
	_          struct{} `cbor:",toarray"`
	ServiceID  string
	GrantID    TURNGrantID
	URLs       []string
	Username   string
	Password   string
	ValidForMS uint64
}

type TURNDemand struct {
	RequestID  uint64
	Mode       TURNMode
	ServiceIDs []string
}

type TURNGrants struct {
	RequestID   uint64
	Status      TURNStatus
	HasFallback bool
	Grants      []TURNGrant
}

func ValidTURNServiceID(id string) bool {
	return len(id) > 0 && len(id) <= MaxTURNServiceIDBytes && strings.TrimSpace(id) == id && utf8.ValidString(id) && norm.NFC.IsNormalString(id)
}

func ValidTURNCredential(value string) bool {
	return value != "" && len(value) <= MaxTURNCredentialBytes && utf8.ValidString(value) && norm.NFC.IsNormalString(value)
}

func (g TURNGrant) Valid() bool {
	if !ValidTURNServiceID(g.ServiceID) || g.GrantID == (TURNGrantID{}) || len(g.URLs) == 0 || len(g.URLs) > MaxURLsPerGrant {
		return false
	}
	for _, v := range []string{g.Username, g.Password} {
		if !ValidTURNCredential(v) {
			return false
		}
	}
	for _, url := range g.URLs {
		if !ValidTURNURL(url) {
			return false
		}
	}
	return true
}

func EncodeTURNDemand(d TURNDemand) ([]byte, error) {
	if d.RequestID == 0 || (d.Mode != TURNParallel && d.Mode != TURNAll) || len(d.ServiceIDs) > MaxGrantsPerMessage {
		return nil, ErrTURNControl
	}
	seen := make(map[string]bool)
	for _, id := range d.ServiceIDs {
		if !ValidTURNServiceID(id) || seen[id] {
			return nil, ErrTURNControl
		}
		seen[id] = true
	}
	ids := d.ServiceIDs
	if ids == nil {
		ids = []string{}
	}
	return EncodeBody([]any{turnSchema, turnDemandKind, d.RequestID, d.Mode, ids})
}

func DecodeTURNDemand(body []byte) (TURNDemand, error) {
	var wire struct {
		_                       struct{} `cbor:",toarray"`
		Schema, Kind, RequestID uint64
		Mode                    TURNMode
		IDs                     []string
	}
	if len(body) > MaxTURNBodyBytes || messageDecMode.Unmarshal(body, &wire) != nil {
		return TURNDemand{}, ErrTURNControl
	}
	d := TURNDemand{wire.RequestID, wire.Mode, wire.IDs}
	encoded, err := EncodeTURNDemand(d)
	if err != nil || !bytes.Equal(encoded, body) {
		return TURNDemand{}, ErrTURNControl
	}
	return d, nil
}

func EncodeTURNGrants(g TURNGrants) ([]byte, error) {
	if g.RequestID == 0 || g.Status < TURNOK || g.Status > TURNRetryLater || len(g.Grants) > MaxGrantsPerMessage || (g.Status != TURNOK && len(g.Grants) != 0) || (g.Status == TURNUnconfigured && g.HasFallback) {
		return nil, ErrTURNControl
	}
	seen := make(map[string]bool)
	issued := make(map[TURNGrantID]bool)
	for _, grant := range g.Grants {
		if !grant.Valid() || seen[grant.ServiceID] || issued[grant.GrantID] {
			return nil, ErrTURNControl
		}
		seen[grant.ServiceID], issued[grant.GrantID] = true, true
	}
	items := g.Grants
	if items == nil {
		items = []TURNGrant{}
	}
	body, err := EncodeBody([]any{turnSchema, turnGrantsKind, g.RequestID, g.Status, g.HasFallback, items})
	if len(body) > MaxTURNBodyBytes {
		return nil, ErrTURNControl
	}
	return body, err
}

func DecodeTURNGrants(body []byte) (TURNGrants, error) {
	var wire struct {
		_                       struct{} `cbor:",toarray"`
		Schema, Kind, RequestID uint64
		Status                  TURNStatus
		Fallback                bool
		Grants                  []TURNGrant
	}
	if len(body) > MaxTURNBodyBytes || messageDecMode.Unmarshal(body, &wire) != nil {
		return TURNGrants{}, ErrTURNControl
	}
	g := TURNGrants{wire.RequestID, wire.Status, wire.Fallback, wire.Grants}
	encoded, err := EncodeTURNGrants(g)
	if err != nil || !bytes.Equal(encoded, body) {
		return TURNGrants{}, ErrTURNControl
	}
	return g, nil
}
