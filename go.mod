module github.com/windshare/windshare

go 1.26.5

// 双模块发版机制(执行计划 §6.2):根模块以版本 require core,禁用 replace
// (含 replace 会令 `go install .../cmd/windshare@latest` 失败)。发版两步:
// ① 先打 core/vX.Y.Z tag;② 将下行升为该版本,再打根模块 tag。本地开发经
// go.work 解析;发布构建(GOWORK=off)按此版本从远端解析。
require github.com/windshare/windshare/core v0.2.1

// WS 库选型:coder/websocket——context 原生 API、零第三方依赖、积极维护
// (nhooyr.io/websocket 的官方延续);gorilla/websocket 处于维护模式且 API
// 无 context 取消语义。
require github.com/coder/websocket v1.8.15

// connectivity/v2signal uses deterministic CBOR for canonical E2EE signaling
// envelopes; pinning it directly keeps the root wire codec explicit and auditable.
require github.com/fxamacker/cbor/v2 v2.9.2

// Pion is isolated in transport/webrtc so core remains transport-neutral. D1
// pins the version proven against Chromium by the accepted S0 interop spike.
require github.com/pion/webrtc/v4 v4.2.16

// Relay endpoint normalization must resolve Unicode hosts exactly as browser
// WHATWG URL does; pinning x/net directly also holds the audited security floor.
require golang.org/x/net v0.57.0 // indirect

// The Windows test runner verifies handle-owned leases and named-pipe guards at
// the OS boundary; this dependency is test-harness-only and already in the graph.
require golang.org/x/sys v0.47.0

// The D5 policy gate resolves actual call targets and control-flow dominance;
// lexical import/name matching cannot prove that a runtime gate guards a socket.
require golang.org/x/tools v0.48.0

require golang.org/x/text v0.40.0

require (
	github.com/google/uuid v1.6.0 // indirect
	github.com/pion/datachannel v1.6.2 // indirect
	github.com/pion/dtls/v3 v3.1.4 // indirect
	github.com/pion/ice/v4 v4.2.7 // indirect
	github.com/pion/interceptor v0.1.45 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/mdns/v2 v2.1.0 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtcp v1.2.16 // indirect
	github.com/pion/rtp v1.10.2 // indirect
	github.com/pion/sctp v1.10.3 // indirect
	github.com/pion/sdp/v3 v3.0.19 // indirect
	github.com/pion/srtp/v3 v3.0.12 // indirect
	github.com/pion/stun/v3 v3.1.6 // indirect
	github.com/pion/transport/v4 v4.0.2 // indirect
	github.com/pion/turn/v5 v5.0.10 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/mod v0.38.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/time v0.14.0 // indirect
)
