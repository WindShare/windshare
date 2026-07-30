# Browser evidence execution

Each hosted suite resolves its current-machine topology before running samples. Main and Pion may have different resolution bytes, but both must bind the same profile digest and topology ID. The selected run policy is the sole sample-count authority.

Samples run as native child processes under a parent-owned process-tree boundary. The child can write only its private attachment capability; it cannot write `result.json` or receive repository secrets. The parent writes a provisional result before launch, waits for the complete process tree to become quiescent, finalizes runner logs into the private collection, and atomically replaces the result with terminal facts.

Guarding is one suite transaction after every sample is terminal. The guard receives explicit secrets out of band, scans canonical result/control bytes, every attachment, and bounded ZIP contents, then copies exact authorized bytes into one private suite bundle. Any `quarantined` or `failed` sample blocks the whole suite upload without changing its runtime result.

A successful guard publishes exactly one deterministic, no-replace sealed upload authority. The suite manifest digest must cross the job boundary separately from the downloaded bytes. Verdict resolution requires that digest, verifies the complete bundle once, and consumes the returned result and guard snapshots without reopening per-sample paths. There is no per-sample publication or artifact-generation authority.

On Windows, the process runner requires the Job Object helper. Native no-replace artifact publication is also used for the final bundle because POSIX mode bits are not an immutability boundary on Windows.

## Validation layers

`make browser-contract` runs Vitest semantics plus every direct `tests/contract/*.tests.mjs` entry in a stable, fail-closed order. Each Node contract receives the child-process guard; real subprocess checks belong to `tests/process`, while Playwright discovery remains owned by each product suite. `make workflow-lint` pins actionlint for GitHub syntax, and the independent YAML-AST contract owns WindShare-specific job, credential, artifact, deadline, and reducer semantics.

`pnpm -C web run test:browser:generated-semantic:process` is the focused hostile-environment boundary. `test:browser:process` composes it with the platform process integrations. `make browser` adds product browsers, and `make ci` remains the complete local mirror.

## Generated semantic artifact

`final-semantic-reducer.js` is produced in memory under the exact `.node-version` and lock-authorized Vite/Rolldown versions. The build disables Vite config, env-file, tsconfig, plugin, ambient-CWD, and Rolldown debug-path discovery; its worker starts from an empty environment and uses absolute paths plus a private cache. Validation admits one fixed entry, the complete semantic digest and exports, and a canonical observed subset of the approved `node:crypto`, `node:path`, and `node:fs/promises` capabilities. A successful `--write` publishes the already-validated bytes through same-directory exclusive staging, sync, close, and atomic rename only after temporary-state cleanup succeeds.

Every runtime build verifies the committed artifact exactly once through the authenticated platform process owner before native builds or reducer loading. Ubuntu and Windows also run the hostile real-process target independently of the browser artifact DAG. External memory limits were implementation-time diagnostic evidence only; raw CLI and CI memory authority remains a separate process-owner hardening concern.
