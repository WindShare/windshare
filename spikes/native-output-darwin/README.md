# Darwin native output experiment

Run via [GitHub Actions](../../.github/workflows/native-output-darwin-experiment.yml) on macOS Intel and Apple Silicon. It records the actual APFS volume and native capability bits; the Go test also exercises the pinned `x/sys/unix` wrappers with CGO disabled.

The bounded probe covers identity across hard links and rename, publication collisions and competing writers, directory replacement, symlink rejection, process locking, and SIGKILL at four data/checkpoint/publication cuts. Fresh processes resume only recorded ranges and reject copied/replaced objects, changed source revisions, and altered recorded bytes. Assertions hash fixture data to verify the experiment, not to prescribe full-file reads during production recovery.

```sh
python3 spikes/native-output-darwin/probe.py --root "$TMPDIR/wind-output-probe" --output evidence.json
go test -count=1 -v ./spikes/native-output-darwin
```

This is native-boundary evidence, not a macOS output implementation or power-loss certification. Persistent identity and cleanup contracts still need implementation review; successful samples do not prove identifier non-reuse. A retained directory handle remains attached to a moved directory, so public placement needs a separate check.

Sources: Apple's [rename contract](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/man/man2/rename.2), [attribute contract](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/man/man2/getattrlist.2), and [attribute definitions](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/sys/attr.h).
