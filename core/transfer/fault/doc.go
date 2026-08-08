// Package fault defines the transport-neutral failure values consumed by
// transfer settlement policy.
//
// Fault intentionally excludes native errors and diagnostic context. Keeping
// those details at the boundary that observed them prevents wrapping topology
// from becoming lifecycle or retirement authority.
package fault
