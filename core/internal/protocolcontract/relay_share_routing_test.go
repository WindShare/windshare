package protocolcontract

import (
	"bytes"
	"slices"
)

type v2RelayShareRoutingVector struct {
	v2RelayEndpointVector
	ShareIDRawB64 string `json:"shareIdRawB64"`
}

func v2RelayShareRoutingCases() []v2RelayShareRoutingVector {
	shareID := []byte{0xfb, 0xff, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	const route = "share_id=-_8AAQIDBAUGBwgJ"
	accepted := func(name, base, dial, identity string, share []byte) v2RelayShareRoutingVector {
		return v2RelayShareRoutingVector{
			Name: name, RelayBaseURL: base, Accepted: true, DialEndpoint: dial,
			RelayIdentityEndpoint: identity,
			RelayIdentityB64:      b64(hash(slices.Concat([]byte(domainRelayIdentity), []byte(identity)))),
			ShareIDRawB64:         b64(share),
		}
	}
	rejected := func(name, base string, share []byte) v2RelayShareRoutingVector {
		return v2RelayShareRoutingVector{
			Name: name, RelayBaseURL: base,
			ShareIDRawB64: b64(share),
		}
	}
	return []v2RelayShareRoutingVector{
		accepted("root", "https://relay.example", "wss://relay.example/v2/ws?"+route, "wss://relay.example/v2/ws", shareID),
		accepted("canonical-base", "HTTPS://RELAY.EXAMPLE:443/base/", "wss://relay.example/base/v2/ws?"+route, "wss://relay.example/base/v2/ws", shareID),
		accepted("query-bytes-and-order", "https://relay.example?token=a%2fb%20c+~&flag&token=two&empty=&", "wss://relay.example/v2/ws?token=a%2fb%20c+~&flag&token=two&empty=&&"+route, "wss://relay.example/v2/ws", shareID),
		accepted("query-values-are-not-keys", "https://relay.example?token=x;share_id=other&next=share_id%3Dx", "wss://relay.example/v2/ws?token=x;share_id=other&next=share_id%3Dx&"+route, "wss://relay.example/v2/ws", shareID),
		accepted("decode-names-once-case-sensitive", "https://relay.example?share%255fid=x&Share_ID=y&share+id=z&%ff=x", "wss://relay.example/v2/ws?share%255fid=x&Share_ID=y&share+id=z&%ff=x&"+route, "wss://relay.example/v2/ws", shareID),
		accepted("ipv6-escaped-path", "https://[2001:db8::1]:443/a%2Fb/", "wss://[2001:db8::1]/a%2Fb/v2/ws?"+route, "wss://[2001:db8::1]/a%2Fb/v2/ws", shareID),
		accepted("local-port", "http://127.0.0.1:8080/relay", "ws://127.0.0.1:8080/relay/v2/ws?"+route, "ws://127.0.0.1:8080/relay/v2/ws", shareID),
		accepted("other-share-same-relay", "https://relay.example", "wss://relay.example/v2/ws?share_id=AQEBAQEBAQEBAQEB", "wss://relay.example/v2/ws", bytes.Repeat([]byte{1}, 12)),
		rejected("matching-preconfigured-route", "https://relay.example?"+route, shareID),
		rejected("conflicting-preconfigured-route", "https://relay.example?share_id=other", shareID),
		rejected("empty-route", "https://relay.example?share_id=", shareID),
		rejected("bare-route-key", "https://relay.example?token=x&share_id", shareID),
		rejected("duplicate-route", "https://relay.example?"+route+"&"+route, shareID),
		rejected("encoded-route-key", "https://relay.example?sh%61re%5fid=other", shareID),
		rejected("mixed-route-keys", "https://relay.example?"+route+"&%73hare_id=other", shareID),
		rejected("invalid-base", "not a URL", shareID),
		rejected("invalid-query-escape", "https://relay.example?token=%xy", shareID),
		rejected("zero-share", "https://relay.example", make([]byte, 12)),
		rejected("short-share", "https://relay.example", shareID[:11]),
		rejected("long-share", "https://relay.example", slices.Concat(shareID, []byte{1})),
	}
}
