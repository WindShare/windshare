# FSA small-file evidence shell

This optional Windows shell owns the canonical workload, native browser orchestration, and baseline/product result contract. Picker and permission replay use the sibling `BrowserNativeUiReplay` module and fail closed when exact window/control identity cannot be proved.

```powershell
node web/scripts/browser-evidence/fsa-small-file/cli.mjs validate-workload
node web/scripts/browser-evidence/fsa-small-file/cli.mjs verify-host --root <materialized-root> --output <verification.json>
node web/scripts/browser-evidence/fsa-small-file/cli.mjs summarize --baseline <baseline-warmup.json> --product <product-warmup.json> --baseline <baseline-1.json> --product <product-1.json> --baseline <baseline-2.json> --product <product-2.json> --baseline <baseline-3.json> --product <product-3.json> --output <summary.json>
web/scripts/browser-evidence/fsa-small-file/run-native-evidence.ps1
web/scripts/browser-evidence/fsa-small-file/run-native-evidence.ps1 -DiagnosticProductOnly
```

Adapters import `content.mjs` to reproduce bytes, `workload.mjs` to authenticate the manifest, and `results.mjs` to validate records. The runner uses one browser/profile and target volume, alternates pure-FSA and product samples, and verifies every output path and digest. A handoff run uses c8, one warm-up pair, and at least three measured pairs; picker wait is excluded.

Product success requires exact `Published` DirectTree output and median authority-to-publication at most 15 seconds. The finite positive product/baseline median ratio is diagnostic only. Further benchmark-specific tuning is intentionally stopped because protecting common-case behavior and maintainability takes priority. `-DiagnosticProductOnly` captures one warm-up plus one measured product run without making a paired target claim. Raw results stay below `evidence/<session-id>/`.

Run focused contracts with `node --test web/scripts/browser-evidence/fsa-small-file/tests/*.test.mjs`.
