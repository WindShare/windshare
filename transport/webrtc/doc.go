// Package webrtc adapts one ordered, reliable Pion DataChannel to the
// transport-neutral session.FrameChannel contract.
//
// Signaling, ICE policy, relay fallback, and peer selection intentionally live
// above this package. Keeping those decisions out of the adapter lets one
// lifecycle state machine own frame bounds, event-driven flow control, terminal
// acknowledgement, and close ordering.
package webrtc
