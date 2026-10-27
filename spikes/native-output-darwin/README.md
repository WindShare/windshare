# Darwin native output experiment

Run via [GitHub Actions](../../.github/workflows/native-output-darwin-experiment.yml) on macOS Intel and Apple Silicon. The bounded probe covers native identity, no-replace publication, collisions, directory replacement, locking, and SIGKILL at four data/checkpoint/publication boundaries.

[Run 40458323644](https://github.com/WindShare/windshare/actions/runs/40458323644) tested commit `8eda26ae1d95cbf7e0808f7ed8e669a1d6098881` on 2026-10-27. Both runners used macOS 15.7.9 and case-insensitive APFS. [Provenance](evidence/run.json) and raw evidence are retained here:

| Runner | Native cases | CGO-disabled Go primitives / CLI build |
| --- | --- | --- |
| Apple Silicon | [14 passed](evidence/arm64/probe.json) | [Passed](evidence/arm64/go-test.log) / [arm64](evidence/arm64/build.txt) |
| Intel | [14 passed](evidence/amd64/probe.json) | [Passed](evidence/amd64/go-test.log) / [amd64](evidence/amd64/build.txt) |

- Hard-link/rename publication retained the same file ID and allocation (8 blocks before/after). Collisions retained downloaded bytes; 12 competing publishers produced one winner and retained 11 losing stages.
- Fresh processes reused 64 KiB, 64 KiB, 128 KiB, and 2 MiB at the four cuts. Already-published output required zero data writes. Replaced files/roots, changed revisions, and corrupted fixture bytes were rejected without mutation.
- `st_gen` was always zero. `ATTR_CMN_GEN_COUNT` changed from 2 to 3 after a same-size overwrite and stayed 3 after restoring mtime. Keep content-change observations separate from persistent object identity; ordinary download writes also change that counter.
- A retained directory handle stayed attached to the moved directory. Public placement therefore needs a separate check against the requested destination.

```sh
python3 spikes/native-output-darwin/probe.py --root "$TMPDIR/wind-output-probe" --output evidence.json
go test -count=1 -v ./spikes/native-output-darwin
```

This supports pursuing APFS process-restart recovery, not declaring macOS output implemented. Fixtures are at most 2 MiB; this does not test power loss, reboot, other filesystems, or complete WindShare downloads. The identity/attribute probe uses Python `ctypes`; the equivalent CGO-disabled Go binding remains implementation work. Successful samples do not prove identifier non-reuse or cleanup ownership. Fixture byte checks verify experimental integrity without prescribing full-file reads during production recovery.

Sources: Apple's [rename contract](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/man/man2/rename.2), [attribute contract](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/man/man2/getattrlist.2), and [attribute definitions](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/sys/attr.h).
