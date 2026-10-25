// Package engine is WindShare's native application boundary. An Engine owns
// aggregate resource limits while each task owns its connections, selected
// source or output, cancellation, and final settlement.
//
// ResolveReceiveLink accepts a URL and optional separate key before task admission.
// Clients handle ErrReceiveKeyRequired with their own input UI; link validation
// and key-conflict decisions are shared by native and RPC callers.
//
// StartShare and StartReceive return tasks whose bounded observation streams
// never control execution. Wait returns the durable final result only after the
// workflow has stopped producers and released resources. Cancel stops one task;
// ShareTask.StopShare explicitly stops a share; Close stops all
// tasks and closes the instance's capacity owner.
//
// TaskResult retains the authoritative Settlement and diagnostic causes. Share
// and receive streams expose its normalized EngineTaskObserved fact once, using
// engine/observation/wire for serialization. Value contains workflow details;
// errors.As can recover a structured ReceiveFailure from Err even without a
// low-level cause. A nil Wait error means the result was retrieved, not that the
// operation succeeded. StopReason records intent independently of the outcome.
//
// Observations expose typed product state through engine/observation. Optional
// request Diagnostics sinks carry source-owned records separately, with engine,
// task and workflow scope. Hosts drain and close their bounded diagnostic stream
// after tasks stop; overflow or a closed sink never changes task settlement.
// Diagnostic names and attributes do not extend the product wire contract.
// Clients own formatting and exit codes; injected ports keep native objects out
// of the application API.
//
// ShareTask.Ready retains first service readiness: at least one relay registered
// and local session admission started. Clients own capability presentation and
// stop the share if presentation fails. Directory prefetch follows readiness in
// the background; relay availability and active sessions evolve independently.
//
// Output and recovery ports are platform-neutral; hosts assemble osfs through
// adapters/filesystem. Provider-owned diagnostic events retain native detail.
//
// InspectRecovery returns listed operation identities after releasing destination
// leases. RecoveryInventory.Discard reacquires output authority and rechecks those
// identities before changing owned state, so a prompt holds no destination lease.
package engine
