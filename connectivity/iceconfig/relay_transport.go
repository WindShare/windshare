package iceconfig

import "github.com/pion/stun/v4"

// RelayTransport describes the client-to-TURN hop, independently of the UDP
// relay candidate advertised to ICE. Ordering is an initial product preference,
// not a claim about the throughput of an established connection.
type RelayTransport string

const (
	RelayTCP RelayTransport = "tcp"
	RelayTLS RelayTransport = "tls"
	RelayUDP RelayTransport = "udp"
)

func (r Relay) Transports() []RelayTransport {
	var result []RelayTransport
	for _, transport := range []RelayTransport{RelayTCP, RelayTLS, RelayUDP} {
		if len(r.transportURLs(transport)) != 0 {
			result = append(result, transport)
		}
	}
	return result
}

// WithTransport narrows authority for both peers without changing the service,
// credential or exit route. Browser ICE does not honor the order of TURN URLs.
func (r Relay) WithTransport(transport RelayTransport) (Relay, error) {
	return NewRelay(r.identity, r.transportURLs(transport), r.credentials)
}

func (r Relay) transportURLs(transport RelayTransport) []string {
	var result []string
	for _, raw := range r.urls {
		uri, err := stun.ParseURI(raw)
		if err != nil {
			continue
		}
		kind := RelayUDP
		if uri.Scheme == stun.SchemeTypeTURNS {
			kind = RelayTLS
		} else if uri.Proto == stun.ProtoTypeTCP {
			kind = RelayTCP
		}
		if kind == transport {
			result = append(result, raw)
		}
	}
	return result
}
