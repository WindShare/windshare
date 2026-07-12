// Package relay implements core/session.FrameChannel over the versioned relay
// WebSocket protocol. A Channel owns both its frame and negotiation-signal
// streams; bounded per-session ingress prevents one application consumer from
// stalling a multiplexed sender connection. Terminal frames remain observable
// before Recv closes, and the shared wire contract comes from relay/protocol.
package relay
