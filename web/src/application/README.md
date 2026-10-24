# Browser application runtime

The receive runtime owns command admission, active execution, retained-task recovery,
and destination handoffs. Output providers remain authoritative for durable lifecycle
transitions, checkpoint validation, and file ownership.

ReceiveRuntimeState holds generation-fenced observations. Active and retained
coordinators consume these facts; UI adapters subscribe and format them. UI snapshots
and action labels never authorize execution. ESLint enforces this dependency direction.

A durable pause or terminal observation does not end the running writer lifetime.
Handoffs await execution settlement and detachment. Picker-bearing commands start
synchronously in the caller's user-action stack.
