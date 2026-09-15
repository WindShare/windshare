// Package webrtc adapts one ordered, reliable Pion DataChannel to the
// transport-neutral session.FrameChannel contract.
//
// Signaling, ICE policy, relay fallback, and peer selection intentionally live
// above this package. Keeping those decisions out of the adapter lets one
// lifecycle state machine own frame bounds, event-driven flow control, terminal
// acknowledgement, and close ordering. The PeerConnection owner forwards a
// permanent connection failure through Channel.Fail even after channel handoff;
// signaling cancellation and temporary disconnection do not end the channel.
package webrtc
