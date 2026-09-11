# Folder recovery browser evidence

Optional, bounded storage evidence using the deterministic content generator from `../fsa-small-file/`.
The default harness is a **raw browser API reference**, not the WindShare delivery runtime. A candidate
module must implement `runCase(input)`, `resumeCase({caseId})`, and `cleanupCase({caseId})`, emitting the
same observations through `globalThis.__folderEvidenceEvent`. The report records each harness identity.

```powershell
node --test web/scripts/browser-evidence/folder-recovery/tests/*.test.mjs
node web/scripts/browser-evidence/folder-recovery/run.mjs --output .tmp/folder-reference.json
web/scripts/browser-evidence/folder-recovery/run-native.ps1 -OutputDirectory .tmp/folder-native-reference
```

The native wrapper uses the existing sibling `BrowserNativeUiReplay` module, an isolated visible Edge
window, a verified picker/permission interaction, and a fresh target under repository `.tmp/`. `-Browser
Chrome` is also available. Headless runs use an OPFS directory as the FSA destination surrogate; they
make no claim about external target filesystems. Profiles are temporary and removed after the run;
native target cleanup is verified before its owned directory is removed. Result files are never overwritten.

The fixed mixed workload has two 24 MiB staged files and two 4 KiB direct files. One stage is received
and copied at a time. A second case aborts a copy after 4 MiB, reloads the page, retries the retained
24 MiB file offline without source reads, verifies its digest, and removes staging. Small-file timing
alternates direct reference and candidate runs in the same browser/profile: one warmup each, three
measured runs each, 32 files of 4 KiB. Use `--repetitions` / `-Repetitions` for 1–5 diagnostic samples.
Storage event sampling is part of the workload; it is not uninstrumented throughput or network speed.

To exercise the production assembly, pass `--candidate /scripts/browser-evidence/folder-recovery/product-harness.mjs`
and `--baseline /scripts/browser-evidence/folder-recovery/direct-product-harness.mjs` to the Node runner,
or `-Candidate` / `-Baseline` to the native wrapper. This compares automatic delivery with the existing
FSA checkpoint/ledger path. The product harness injects a reliable 1 KiB/s receipt rate so bounded
24 MiB files exercise the real long-receive planner without waits. Generated local bytes never update
that synthetic network rate; copy and flush costs, transaction lifetimes and case timings use real clocks.
It exercises production output modules directly, not network transfer, full UI wiring, or real operation-lease acquisition.

Reports distinguish:

- Browser-enumerated stage and target file lengths, including any visible temporary entries.
- Host file lengths and filesystem-reported allocated blocks from the profile and, for native runs,
  the external target. Host scans run every 25 ms when idle and at storage milestones. They are
  non-atomic peak estimates: scans can miss brief copies or combine states from different instants.
  They are not strict bounds on simultaneous peak. Allocation does
  not establish unique physical extents, volume usage, or space available to a production user.
- Cumulative target writes and local-copy bytes, including failed attempts. These are cumulative
  write quantities, not simultaneous disk occupancy.

Per-root maxima can occur at different times; never add them to claim a simultaneous peak. Browser
profiles include caches and databases unrelated to file payload. Each case is bounded to 64 MiB and
host-observed profile plus target has a 1 GiB stop guard. These diagnostics do not certify large-file
performance, power-loss durability, NAS behavior, or every supported browser. Unit tests and the
ordinary browser contracts continue to own correctness.
