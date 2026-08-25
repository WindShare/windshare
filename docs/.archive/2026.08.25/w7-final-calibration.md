# W7 final calibration

## Rationale and retained architecture

- The accepted product target is 15 seconds and maintainability is the priority. The verified single-batch
  c3-inspection sample was 12.657/13.287/14.589 seconds, so cross-batch preparation no longer has a product
  justification.
- Windows Chromium FSA retains c15 file pipelines, c8 native writers, and c3 initial-claim inspections. The
  generic persistent-tree default remains c1 inspection.
- Each immediately started batch contains at most 64 already-arrived claims. It performs one c1 lineage
  classification, one bounded ordered inspection group, one c1 occupied-destination reclassification, and
  one c1 atomic claim installation. A failure stops further admission and drains accepted siblings before
  propagating the first discovery-order error.
- The cross-batch prepared-context queue, depth options/constants, FSA propagation, diagnostics setting, and
  preparation-only tests were removed. The reason-coded inspector projection now reports
  `batch_serialization` instead of implying a configurable prepared-context limit; native evidence records
  `productInitialClaimBatchMode: 'single-batch'`.
- Claim-before-create, scheduler same-parent serialization, different-parent overlap within the bounded
  group, first-file no-delay, and all checkpoint/durability identity semantics are unchanged.

## Verification

- Focused preparation/route/diagnostics: 66/66 tests pass.
- Full retained lifecycle/FSA matrix: 171/171 tests pass.
- TypeScript, focused ESLint, `make sloc`, and `git diff --check` pass.
- `make check` passes: 201 web test files / 1,653 tests plus production short Go suites.

## Blockers

- None.
