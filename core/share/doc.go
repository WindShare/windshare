// Package share composes authenticated manifests, packed-stream geometry, and
// injected file IO into sender and receiver facades. Options can inject the read secret
// and random source that make nonce-bearing vectors deterministic; key derivation is
// wired here through core/internal/keyderiv.
//
// A Receiver is inert until Plan compiles selection exactly once. TransferPlan then
// owns canonical selected entries, compact chunk demand, selected bytes, deterministic
// PlanID, selective block materialization, have-state, and finalization. Boundary chunks
// may be downloaded in full, but their unselected sibling ranges never reach FileSink.
//
// Sharer.BlockStore/Sealer and TransferPlan.Sink plus Receiver.Opener expose narrow
// session dependencies without importing any transport implementation.
package share
