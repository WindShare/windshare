// Package connectivity composes session-scoped signaling with transport-neutral
// frame channels. It owns WebRTC negotiation, share-level sender fan-out, and
// fingerprint-gated receiver recovery, while relay and WebRTC adapters remain
// unaware of fallback, fan-out, or scheduler policy.
package connectivity
