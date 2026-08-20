# Browser Output Failure Diagnostics

## Status

The destination-path compatibility hypothesis is blocked. The historical captures show a repeatable local state_io failure, but they do not identify a rejected browser API or an incompatible path. Current Edge and Chrome host probes successfully created and wrote the tested .git/hooks path, so WindShare must not infer incompatibility from that path, an exception name, or diagnostic text.

Evidence:

- The [Windows host-path probe](../../web/windows-fsa-path-probe.html) completed the `.git/hooks` create, writer-open, write, and durable-reopen sequence in current Windows Chrome and Edge after root-edit permission was granted.
- The historical captures contain only aggregate `checkpoint`/`state_io` incidents before range acknowledgement; they do not contain a correlated FSA method, IndexedDB transaction, target entry, writer, permission, or committed-byte observation.
- The [stage fault matrix](../../web/test/output/persistent-stage-diagnostics.test.ts) now distinguishes the relevant FSA and IndexedDB boundaries while preserving the original exception and failure-time side-effect facts.

## Current Direction

- Preserve DirectTree and its existing output-wide state_io pause behavior.
- Observe the exact failing File System Access or IndexedDB stage, retain the raw local exception, and rethrow the same value through the existing transfer policy.
- Keep failure-time observations local, bounded, and diagnostic-only. They must not select retry, omission, fallback, or another recovery action.
- Perform no compatibility probes, extra requests, decision writes, or permission prompts on the successful path.
- Keep writer-close ownership explicit until close settles, and release a bound materialization when activation fails.
- Keep the Windows FSA probe as a manual evidence collector outside ordinary test discovery.

Destination decisions, request fencing, omission/outcome models, replacement roots, and fallback successors remain deferred until a supported browser produces a correlated real rejection with enough post-failure evidence to prove a safe recovery boundary.
