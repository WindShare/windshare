// Package engine is WindShare's native application boundary. An Engine owns
// aggregate resource limits while each task owns its connections, selected
// source or output, cancellation, and final settlement.
//
// StartShare and StartReceive return tasks whose bounded observation streams
// never control execution. Wait returns the durable final result only after the
// workflow has stopped producers and released resources. Cancel stops one task;
// ShareTask.StopShare explicitly stops a share; Close stops all
// tasks and closes the instance's capacity owner.
//
// TaskResult and the finished LifecycleObservation share one Settlement containing
// Outcome, FailureClass, normalized Failure/CleanupFailure, and diagnostic causes.
// Value contains workflow details;
// errors.As can recover a structured ReceiveFailure from Err even without a
// low-level cause. A nil Wait error means the result was retrieved, not that the
// operation succeeded. StopReason records intent independently of the outcome.
//
// Observations use engine/observation facts. Native traces are normalized before
// publication; clients own formatting and exit codes. Constructors accept small
// capabilities so tests can supply connections and peers without native objects.
//
// ShareTask.Ready retains first service readiness: at least one relay registered
// and local session admission started. Clients own capability presentation and
// stop the share if presentation fails. Directory prefetch follows readiness in
// the background; relay availability and active sessions evolve independently.
//
// InspectRecovery returns listed operation identities after releasing filesystem
// leases. RecoveryInventory.Discard reacquires output authority and rechecks those
// identities before changing owned state, so a prompt holds no filesystem lease.
package engine
