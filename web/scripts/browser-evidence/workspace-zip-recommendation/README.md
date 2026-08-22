# Workspace ZIP recommendation evidence model

This directory retains the deterministic candidate derivation and the benchmark specification that explain
the reviewed `workspace-then-publish` recommendation threshold. Browser/process acquisition code and raw run
receipts are intentionally not part of the shipping tree.

The fixed matrix is three sequential repetitions at 32, 128, 256, and 512 MiB. A 1 GiB case is considered
only when the measured curve has no space or responsiveness boundary. The out-of-process sampler enforces a
1.25 GiB owned-data cap and a 16 GiB untouched volume reserve. Evidence never writes Downloads or native FSA
fixtures. The runner always emits an unreviewed candidate; the retained final run
`20260822T090422Z-40945d81dc004040` was separately reviewed without rewriting that candidate. Its three
512 MiB cases freeze an inclusive `1,073,744,986`-byte checked peak recommendation for only the exact reviewed
Windows/Edge/NTFS support row. The 1 GiB raw case was not run because its exact modeled peak
`2,147,489,972` bytes exceeds the predeclared 1.25 GiB evidence cap. Recommendation remains display ranking
after complete exact discovery, checked budget, and route availability; it performs no scan or authority action.

`benchmark-spec.md` records the measurement contract used by the completed experiment. `candidate.mjs` remains
a pure verifier for the three-repeat scale and next-scale boundary; it neither launches a browser nor grants
production support.

```powershell
node --test web/scripts/browser-evidence/workspace-zip-recommendation/tests/*.test.mjs
```
