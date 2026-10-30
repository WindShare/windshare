# Public-API SCTP lost-retransmission reproduction

Baseline: [pion/sctp `6ec762e`](https://github.com/pion/sctp/commit/6ec762ebc1106ae8c5ce1a69208a98dfcfdddaaf), checked 2026-10-30. Candidate: [candidate.patch](candidate.patch), also applied on the fork's `fix/rack-lost-retransmissions` branch.

## Finding

Both upstream RACK scans exclude previously retransmitted chunks. A reliable ordered stream continues sending while a UDP wrapper drops one target TSN's original transmission and first retry. Later transmissions are acknowledged, yet upstream recovers the target through T3. The candidate admits outstanding retries using their latest send time and rejects implausibly early retry ACKs as RACK timing evidence.

The reproducer uses only public SCTP construction, stream, logging and shutdown APIs. It understands DATA/I-DATA and SACKs, preserves whole datagrams, records collateral drops when chunks are bundled, and verifies every echoed payload. Each endpoint adds 10 ms of network delay by default; protocol timers and congestion settings retain their defaults.

## Results

Final batches: **102 successful transfers** — Windows loopback 52, Ubuntu loopback 44, Windows → Ubuntu LAN 6. Every final run verified payloads; both LAN processes exited successfully.

The table measures time from the deliberately lost first retry to the next transmission of that TSN, not total transfer duration.

| Double-loss environment | Runs per variant | Upstream | Candidate | T3: upstream → candidate |
| --- | ---: | ---: | ---: | ---: |
| Windows loopback | 5 | 971–976 ms | 21–29 ms | 5 → 0 |
| Ubuntu loopback | 5 | 970–974 ms | 21–32 ms | 5 → 0 |
| Windows → Ubuntu LAN | 3 | 966–973 ms | 33–56 ms | 3 → 0 |

The matrix also covers three consecutive losses, single/no loss, short flights, tail loss, reordering, late originals, DATA versus I-DATA, 50 ms nominal RTT, and fragmented messages. Three-loss baseline runs needed two T3 expirations; repaired runs needed none. The original tail case drops both the final DATA transmission and its TLP retry, leaving T3 to recover it in both variants. Single tail loss can recover through TLP, as the follow-up below confirms.

This is recovery evidence, not a throughput claim. Linux traces provide explicit later-send ACK witnesses before recovery in every primary repaired run. Some Windows runs have no strictly later *wire-observed* timestamp witness; their outcomes and RACK logs are retained, but do not independently establish send ordering. The late-original scenario completed successfully but delayed ACKs did not exercise the minimum-RTT rejection branch; dedicated unit tests cover that branch.

## Tail boundary follow-up

**96 additional transfers passed payload verification:** Windows 48, Ubuntu 48. Each platform ran both original binaries at nominal RTTs of 20/50 ms, dropping 0/1/2/3 transmissions of one unfragmented final message, three repetitions per combination. Binary hashes match the original evidence. Both variants followed the same paths; every run had zero later-send ACK witnesses and zero target RACK loss marks.

| Consecutive target losses | Recovery path | Original send → successful retransmission |
| --- | --- | ---: |
| 0 | No retransmission | — |
| 1: original | One TLP, no T3 | 250–322 ms |
| 2: original + TLP | One TLP, then one T3 expiry | 1000–1001 ms |
| 3: original + TLP + first T3 retry | One TLP, then two T3 expiries | 3000–3003 ms |

These times start at the **original send**, unlike the earlier table, and exclude delivery/ACK latency. Timer logs and event sequence establish each retransmission's trigger. With one outstanding chunk, this implementation adds a 200 ms delayed-ACK allowance to `2*SRTT` before probing. RACK needs later delivery evidence; TLP can solicit feedback, but a lost probe without new feedback falls back to T3. [RFC 8985 §7.3](https://www.rfc-editor.org/rfc/rfc8985.html#section-7.3) describes that fallback for TCP; [RFC 9260 §6.3](https://www.rfc-editor.org/rfc/rfc9260.html#section-6.3) defines SCTP's T3 safety net. The exact timer-rearming policy remains the separate [#498](https://github.com/pion/sctp/pull/498) topic.

Run `python run_tail.py --output results/tail-followup-windows` (use `tail-followup-linux` on Linux). Collect both directories, then run `python summarize_tail.py`. The runner requires a new output directory and the original binary hashes; the audit checks payload completion, fault counts, timer ordering and trace hashes. [tail-summary.json](tail-summary.json) and [tail-evidence.zip](tail-evidence.zip) retain the new evidence separately from the frozen 102-run archive.

Further latency work should measure real DataChannel traffic, delayed/lost ACKs and TLP/T3 interaction. These isolated DATA-loss experiments do not justify lowering timers or sending repeated probes without feedback.

## Run

Requires Go; measurements used Go 1.27.3 on Windows and Ubuntu. This directory is an independent module and imports no WindShare implementation.

```powershell
go run . -trace results/upstream.jsonl
./build.ps1 -ForkPath ../sctp
./.build/repaired.exe -trace results/repaired.jsonl
python run_matrix.py --output results/new-run
```

Use a fresh output directory. On Linux, build with `go build -o .build/upstream-linux .`; use a separate modfile with a local SCTP replacement for the candidate. `run_lan.py --ssh-host RECEIVER --address LAN_IP --remote-dir /workspace/reproducer --output results/new-lan` runs an unchanged upstream receiver using explicit lab configuration. Each run logs its configuration, TSN attempts, SACKs, Pion recovery events and completion.

## Validation and evidence

- The 26 new regression subcases cover both scanners, repeated/recent/queued/acknowledged/abandoned chunks, cumulative/selective ACK ambiguity and Karn's rule. **10 fail on upstream; all pass with the candidate.**
- Full `go test -race -count=1 ./...` passed on Windows (40.6 s) and Ubuntu (41.9 s). The final public-API harness also passed a race-instrumented run and `go vet`.
- Full upstream-pinned golangci-lint **v2.10.1** passed using **Go 1.26.9 and LF-normalized source copies**. Its importer cannot read Go 1.27 export data; Windows CRLF checkouts also trigger existing GCI failures. No unrelated source formatting was committed to the fork.
- Exploratory failures remain archived: early harness shutdown errors and a unit-fixture timer leak were corrected before final batches. The late-original scan-only experiment is not evidence that the RTT guard was exercised.

[summary.json](summary.json) contains aggregates and source hashes. [evidence.zip](evidence.zip) retains final/exploratory traces, per-run commands, validation logs and the candidate patch. `python summarize.py` rebuilds the archive from local results.

Published archives replace workstation paths and LAN addresses with portable paths and documentation addresses; runners require explicit SSH configuration. Measurements and binary hashes are unchanged. Each archive's `publication.json` records original/published digests, and references are updated to the published bytes. Raw captures stay in ignored `results/`. Run `python -m unittest public_evidence_test` to check publication behavior.
