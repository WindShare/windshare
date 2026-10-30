module github.com/windshare/windshare

go 1.27.3

// The pinned provider adaptation is reproduced and verified by third_party/pion.
replace github.com/pion/ice/v4 => ./third_party/pion/ice

replace github.com/pion/webrtc/v4 => ./third_party/pion/webrtc

replace github.com/pion/turn/v5 => ./third_party/pion/turn

replace github.com/pion/sctp => ./third_party/pion/sctp

require (
	filippo.io/edwards25519 v1.2.0
	// WS 库选型:coder/websocket——context 原生 API、零第三方依赖、积极维护
	// (nhooyr.io/websocket 的官方延续);gorilla/websocket 处于维护模式且 API
	// 无 context 取消语义。
	github.com/coder/websocket v1.8.15
	// connectivity/v2signal uses deterministic CBOR for canonical E2EE signaling
	// envelopes; pinning it directly keeps the root wire codec explicit and auditable.
	github.com/fxamacker/cbor/v2 v2.9.2
	github.com/pion/ice/v4 v4.4.2
	github.com/pion/logging v0.2.4
	github.com/pion/stun/v4 v4.0.0
	github.com/pion/transport/v4 v4.1.0
	github.com/pion/turn/v5 v5.1.0
	// Native transport and connectivity own Pion; core remains transport-neutral.
	// The pinned source projection preserves provider capabilities across upgrades.
	github.com/pion/webrtc/v4 v4.2.20
	// TerminalCanvas clips only at grapheme boundaries and measures combining,
	// East Asian wide, and emoji clusters in terminal display cells.
	github.com/rivo/uniseg v0.4.7
	golang.org/x/mod v0.38.0
	// The Windows test runner verifies handle-owned leases and named-pipe guards at
	// the OS boundary; this dependency is test-harness-only and already in the graph.
	golang.org/x/sys v0.47.0
	// Destructive resume confirmation must distinguish a real terminal from pipes,
	// files, and other redirected character devices on every supported platform.
	golang.org/x/term v0.45.0
	golang.org/x/text v0.40.0
)

require (
	github.com/google/uuid v1.6.0 // indirect
	github.com/pion/datachannel v1.6.2 // indirect
	github.com/pion/dtls/v3 v3.1.8 // indirect
	github.com/pion/interceptor v0.1.48 // indirect
	github.com/pion/mdns/v2 v2.2.0 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtcp v1.2.17 // indirect
	github.com/pion/rtp v1.10.5 // indirect
	github.com/pion/sctp v1.11.1 // indirect
	github.com/pion/sdp/v3 v3.0.19 // indirect
	github.com/pion/srtp/v3 v3.0.13 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	// Relay endpoint normalization must resolve Unicode hosts exactly as browser
	// WHATWG URL does; pinning x/net directly also holds the audited security floor.
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/time v0.14.0 // indirect
)
