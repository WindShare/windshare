# Browser evidence execution

Each hosted suite resolves its current-machine topology before running samples. Main and Pion may have different resolution bytes, but both must bind the same profile digest and topology ID. The selected run policy is the sole sample-count authority.

Samples run as native child processes under a parent-owned process-tree boundary. The child can write only its private attachment capability; it cannot write `result.json` or receive repository secrets. The parent writes a provisional result before launch, waits for the complete process tree to become quiescent, finalizes runner logs into the private collection, and atomically replaces the result with terminal facts.

Guarding is one suite transaction after every sample is terminal. The guard receives explicit secrets out of band, scans canonical result/control bytes, every attachment, and bounded ZIP contents, then copies exact authorized bytes into one private suite bundle. Any `quarantined` or `failed` sample blocks the whole suite upload without changing its runtime result.

A successful guard publishes exactly one deterministic, no-replace sealed upload authority. The suite manifest digest must cross the job boundary separately from the downloaded bytes. Verdict resolution requires that digest, verifies the complete bundle once, and consumes the returned result and guard snapshots without reopening per-sample paths. There is no per-sample publication or artifact-generation authority.

On Windows, the process runner requires the Job Object helper. Native no-replace artifact publication is also used for the final bundle because POSIX mode bits are not an immutability boundary on Windows.
