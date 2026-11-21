package networkprobe

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"

	"github.com/coder/websocket"
	"github.com/windshare/windshare/core/nodedirectory"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

type Target struct {
	OperationID string             `json:"operation_id"`
	Node        nodedirectory.Node `json:"spec"`
}

type Observation struct {
	OperationID string `json:"operation_id"`
	TCP         bool   `json:"tcp"`
	TLSExpires  *int64 `json:"tls_expires"`
	HTTPS       bool   `json:"https"`
	Relay       *bool  `json:"relay"`
	STUN        *bool  `json:"stun"`
}

// Observe keeps the UDP and HTTPS paths independent so a broken reverse proxy
// cannot withdraw a working STUN server.
func Observe(ctx context.Context, client *http.Client, target Target, origins []string) Observation {
	return observe(ctx, client, target, origins, binding)
}

func observe(ctx context.Context, client *http.Client, target Target, origins []string, bind func(context.Context, string) error) Observation {
	node := target.Node
	report := Observation{OperationID: target.OperationID}
	if node.HasRole("relay") {
		report.Relay = new(bool)
	}
	if node.HasRole("stun") {
		report.STUN = new(bool)
	}
	base, err := relayOrigin(node.Relay)
	if err != nil {
		return report
	}
	node.Relay = base.String()
	// UDP and HTTPS each get the full node deadline, even when the other path is black-holed.
	var stun chan bool
	if report.STUN != nil {
		stun = make(chan bool, 1)
		go func() { stun <- checkSTUN(ctx, base.Hostname(), node.STUN, bind) == nil }()
	}
	observeHTTPS(ctx, client, node, origins, &report)
	if stun != nil {
		*report.STUN = <-stun
	}
	return report
}

func observeHTTPS(ctx context.Context, client *http.Client, node nodedirectory.Node, origins []string, report *Observation) {
	var connected atomic.Bool
	var expires atomic.Int64
	trace := &httptrace.ClientTrace{
		ConnectDone: func(_, _ string, err error) {
			if err == nil {
				connected.Store(true)
			}
		},
		GotConn: func(info httptrace.GotConnInfo) {
			connected.Store(true)
			if conn, ok := info.Conn.(*tls.Conn); ok {
				state := conn.ConnectionState()
				if len(state.VerifiedChains) > 0 {
					expires.Store(state.PeerCertificates[0].NotAfter.Unix())
				}
			}
		},
	}
	desc, err := descriptorAt(httptrace.WithClientTrace(ctx, trace), client, node)
	report.TCP = connected.Load()
	if timestamp := expires.Load(); timestamp != 0 {
		report.TLSExpires = &timestamp
	}
	if err != nil {
		return
	}
	report.HTTPS = true
	if report.Relay == nil || !desc.Services.Relay || len(origins) != OriginCount {
		return
	}
	var id v2.ShareID
	rand.Read(id[:])
	endpoint, err := v2.RelayEndpointForShare(node.Relay, id)
	if err != nil {
		return
	}
	for _, origin := range origins {
		// The dialer owns and closes the HTTP handshake response.
		conn, _, err := websocket.Dial(ctx, endpoint.DialURL, &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Origin": {origin}}}) //nolint:bodyclose
		if err != nil {
			return
		}
		_ = conn.CloseNow()
	}
	*report.Relay = true
}
