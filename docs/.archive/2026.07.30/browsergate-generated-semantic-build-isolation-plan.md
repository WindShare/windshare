# Browsergate Generated Semantic Build Isolation Plan

- Status: archived — implemented and validated on 2026-07-30
- Historical role: P0 correctness prerequisite for verifier execution, `make browser`, `make ci`, and completion of the related plan
- Durable invariants: [Browser evidence execution](../../../web/scripts/browser-evidence/README.md#generated-semantic-artifact)

## Decision

Make the generated reducer a deterministic, environment-isolated build artifact. The real verifier remains a runtime preflight, not a contract test; only its pure configuration, artifact, and orchestration logic enters contract discovery.

This P0 fixes the isolation defect. External memory tooling is a one-time validation guard, not a product, local-development, or CI dependency; native memory authority remains separate owner hardening.

## Evidence

On 2026-07-30, the default Vite config loader followed by the SSR build exhausted host commit through native allocation. A 512 MiB Windows Job comparison failed on the current path but completed in about 0.5 seconds with `configFile: false`. The small, acyclic reducer graph and successful isolated build rule out graph size and the React plugin by themselves.

The isolated output also differs from the committed artifact, so regeneration is part of this P0.

## Build contract

The build worker may observe only the reducer graph, build-owned TypeScript transform settings, the repository-pinned exact Node version, and lockfile-resolved Vite/Rolldown versions. It must not observe project Vite config, application tsconfig, `.env` files, inherited build-affecting environment, ambient working-directory changes, application plugins, or Vitest policy.

The effective Vite configuration must include:

```js
{
  root: webRoot,
  configFile: false,
  envDir: false,
  mode: 'production',
  publicDir: false,
  cacheDir: isolatedCacheRoot,
  clearScreen: false,
  logLevel: 'silent',
  build: {
    target: 'es2023',
    ssr: semanticEntry,
    write: false,
    minify: false,
    sourcemap: false,
    rolldownOptions: {
      tsconfig: false,
      experimental: {
        attachDebugInfo: 'none',
      },
      output: {
        format: 'es',
        entryFileNames: 'final-semantic-reducer.js',
      },
    },
  },
}
```

`configFile: false` and `rolldownOptions.tsconfig: false` are both mandatory: the former disables Vite config discovery, while the latter disables Rolldown tsconfig discovery. All remaining transform policy is inline and build-owned.

The thin CLI parses arguments before creating temporary state, importing Vite, or starting the worker. It then starts one internal worker with an environment constructed from an empty map. The worker receives only `NODE_ENV=production`, `TZ=UTC`, `LANG=C`, `LC_ALL=C`, isolated temporary-directory variables, and Windows `SystemRoot`/`WINDIR` where required; executable and module paths are absolute, so `PATH` is absent. Keep this environment constructor pure and shared by direct and owner-managed execution.

Use absolute paths instead of `process.chdir`, and clean temporary state in `finally`. Add a root `.node-version` containing an exact patch version; every CI Node setup consumes it through `node-version-file`, and local validation rejects a different `process.version`.

## Artifact contract

The injected builder returns one in-memory entry chunk. Before comparison or publication, require:

- exactly one JavaScript output named `final-semantic-reducer.js`, with no assets or secondary chunks;
- no dynamic imports and a canonical, duplicate-free observed subset of the approved `node:crypto`, `node:path`, and `node:fs/promises` external imports, using bundler metadata rather than source regexes;
- the complete 64-hex semantic digest and the expected export surface;
- the exact committed generated-directory surface.

Verify mode compares the validated bytes with the committed artifact. `--write` atomically publishes those same bytes only after every policy check passes; failed validation or publication must preserve the previous artifact.

Delete the hand-maintained `final-semantic-reducer.d.mts`; it has no typed consumer and is not independently derivable from the validated JavaScript. The generated payload is one JavaScript file. Build implementation and test files have a separate explicit directory allowlist.

Independent Windows and Linux builds from the same commit, exact Node version, and lockfile must be byte-identical.

## Execution and outcomes

The parser, worker, validator, and publisher return typed outcomes for usage, build, artifact-policy, stale-output, publication, and cleanup failures. The CLI emits exactly one versioned UTF-8 JSON result record. Orchestration rejects missing, duplicate, malformed, trailing, or schema-incompatible records before combining the parsed result with existing spawn, deadline, and process-tree settlement evidence.

Each `build-runtime` invocation executes the verifier exactly once through the authenticated owner before native batch builds. Hosted main and Pion jobs are separate invocations and therefore each execute it once. The build-runtime entrypoint must not load the committed reducer before this preflight settles.

The command router must use lazy command imports. `build-runtime` never imports the reducer because it does not consume it; local reducer consumers may import it only after the successful preflight.

Emit only three structured milestones under `browser-runtime-generated-semantic-preflight`: started, artifact validated, and settled. Include mode, resolved tool versions, byte length, SHA-256, elapsed time, and typed failure context where applicable.

## Test placement

- `tests/contract`: injected builder/filesystem tests for config, CLI parsing, output policy, atomic-publication decisions, typed outcomes, and exactly-once orchestration. These tests may not spawn the verifier.
- `tests/process`: the real isolated build against hostile Vite config, `.env`, `VITE_*`, Node bootstrap environment, CWD, and tsconfig fixtures.
- runtime integration: one preflight before runtime builds through the authenticated owner.
- artifact reproducibility: `--write` followed by two read-only verifier processes.

A dedicated generated-semantic process target runs in an Ubuntu browser-process job and is composed into the existing Windows browser-process job. `make browser` runs it once before runtime construction, so `make ci` inherits the same regression gate. Contract discovery never runs it.

The related contract/workflow plan may establish its runner, directories, Make targets, lint, and YAML contracts while this P0 is open. Only its full browser validation and final completion wait for this plan.

## Implementation sequence

1. Allow the related plan to establish `make browser-contract` and its fail-closed contract runner.
2. Pin one exact Node version across local and CI entrypoints. Add pure regression contracts and hostile process fixtures without running the defective path.
3. Refactor the clean-environment worker, isolated in-memory builder, artifact validator, typed result, and atomic publisher; delete the hand-maintained declaration file.
4. Run the first real Windows and Linux builds once inside external diagnostic memory bounds and compare their bytes; do not retain the tool as a project dependency.
5. Regenerate once, then verify in independent Windows and Linux processes and review the generated diff.
6. Wire the cross-platform process owner, then run `make browser-contract`, the generated-semantic process target, `make browser`, and `make ci`; complete the related plan's final validation.

## Acceptance criteria

- Project Vite config, `.env`, inherited build environment, CWD, and application tsconfig policy cannot affect the worker; the effective config disables both Vite config and Rolldown tsconfig discovery.
- No verifier code changes process working directory, and invalid CLI input starts no worker and performs no Vite or temporary-filesystem work.
- Artifact validation rejects extra outputs, imports outside the approved Node capability set, dynamic imports, an incorrect full digest, and an incorrect export or directory surface.
- `--write` publishes only validated bytes and preserves the prior artifact on failure.
- The hand-maintained declaration file is absent; Windows and Linux builds under the pinned Node/Vite/Rolldown versions are byte-identical, and read-only verification leaves the worktree unchanged.
- Runtime contracts prove one preflight per `build-runtime` invocation and no reducer load before preflight.
- Hostile generated-semantic process fixtures are blocking on both Ubuntu and Windows CI and through local `make browser`.
- `make browser-contract`, `pnpm -C web run test:browser:process`, `make browser`, and `make ci` pass without reproducing host exhaustion.

## Follow-up

Track operation-scoped native memory authority separately in the existing Windows Job/Linux owner framework. That work must freeze platform-specific metrics, authority availability, typed limit evidence, and empty-tree settlement; it must not expand this build module or move the verifier into contract discovery.

Durable invariants now live in the maintained browser evidence documentation; this archived plan remains the implementation record.
