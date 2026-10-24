# Browser application runtime

`browser-share/` assembles catalog storage, relay admission, reconnect supervision,
and transfer discovery. `browser-receive/` coordinates installed FSA, workspace,
portable, Direct ZIP, and retained-operation routes. Storage implementations,
journals, checkpoint validation, and file ownership remain in `output/`.

`BrowserReceiveApplication` in `receive/application.ts` assembles the receive coordinators, exposes
task commands and read-only observations, and owns their shutdown. UI adapters
subscribe and format facts; UI snapshots and action labels never authorize execution.
`diagnostics/` connects application observations to the diagnostic services.

Durable pause or terminal observations do not end a running writer's lifetime.
Handoffs await execution settlement and detachment. Picker-bearing commands start
synchronously in the caller's user-action stack.

The resolved dependency gate keeps application and reusable modules independent
of UI/React, keeps protocol contracts below runtime implementations, and prevents
UI code from assembling output routes or reaching storage ownership implementations.
