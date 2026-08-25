# Browser output name-rejection repair

## Problem

Trace 5 failed before content transfer when Chromium rejected `pyvenv.cfg` at
`fsa.file.entry.inspect`. The lookup created no entry, checkpoint, writer, or acknowledged range,
but concurrent workers later changed settlement evidence. The repair must handle the browser-only
name refusal and make settlement quiescent.

The catalog already excludes non-portable Windows names. Chromium can reject otherwise portable
path components according to browser policy, so observed runtime behavior is authoritative;
extensions, messages, browser brands, and operating systems are not.

## User decisions — do not remove or weaken

The following are explicit user product decisions. Implementation and later plan cleanup must not
remove, weaken, or replace them without a new user decision:

- Enter compatible-name handling only when the awaited real native `getFileHandle()` or
  `getDirectoryHandle()` non-creating lookup itself rejects with the classified `TypeError`. Never
  infer rejection from a filename, extension, error message, browser brand, or operating system.
- Saving the directory with compatible physical names plus a name-restoration tool is an accepted
  product trade-off.
- Compatible names use `<bounded-readable-prefix>.windshare-<six-lowercase-base32>`. The six
  characters are one operation-scoped token generated and persisted for the receive operation. All
  compatible names in that operation normally use the same token across retry, reload, and resume.
  Only a candidate that is actually occupied uses a different deterministic fallback token.
- A compatible name can theoretically equal a real filename. This probability is accepted as very
  low, so collision handling must remain a small bounded linear retry and must not introduce a
  rename graph, recursive remapping, or a general namespace-virtualization framework.
- The restoration script must be safe to execute repeatedly: it never overwrites or deletes output,
  resumes from path state after interruption, and stops on conflicting or missing states.
- The restoration tool must keep a simple sidecar architecture: one immutable script plus one
  adjacent versioned sidecar containing only a header, committed mapping records, and rolling
  checkpoint footers. The latest complete checkpoint normally remains executable before terminal
  settlement. Do not add digests, a mutable per-entry journal, OS identity proof, or self-deletion.
- Sidecar publication must not block or serialize content workers. It runs through one background
  coalescing writer; only terminal settlement or explicit Stop waits for it to catch up.

## Product behavior

DirectTree automatically saves a rejected component under a compatible physical name and continues
the transfer. Before creating the first compatible entry, it prepares a small operation-owned
name-restoration script and sidecar. This is the accepted trade-off: affected output is immediately
usable but does not have its exact logical namespace until the user runs the script.

The change must not be silent. Show a persistent notice after the first replacement and qualify the
terminal result as “completed with compatible names” or “partial with compatible names”. Keep ZIP
and native CLI receive as explicit user choices; never switch artifact type automatically.

Ordinary receives keep their current picker, no-replace, rollback, resume, progress, and publication
behavior. They perform no extra compatible-candidate lookup, mapping persistence, or restoration
artifact creation. The resolver, ledger, projector, and restoration pair are created lazily only
after a classified native lookup rejection.

## Design

### Preserve ordinary output choices

Do not change the existing single-file or directory actions for this repair. In particular, do not
introduce a precreated `showSaveFilePicker()` target: it would change complete-artifact,
no-replace, rollback, and resume guarantees for every single-file receive. Picker cancellation
remains cancellation. ZIP remains an explicit artifact and is never an automatic fallback.

### Classify only a proven pre-mutation refusal

Introduce `PathComponentRejectedError` at the FSA namespace boundary. Wrap only a genuine
`TypeError` from a non-creating `getFileHandle()` or `getDirectoryHandle()` call on a verified
parent with one canonical logical component and an expected entry kind. Retain the original value
as `cause`.

Probe the expected entry kind first. Only after its `NotFoundError` may the opposite-kind lookup
check occupation; a failure from that secondary lookup is never a compatible-name trigger.

Do not inspect names, extensions, messages, browsers, or operating systems, and do not absorb
permission, cancellation, ownership, collision, checkpoint, writer, or contract failures. The
classified call proves only that WindShare did not mutate the rejected destination component.

### Allocate a compatible physical name

The FSA backend owns one operation-scoped resolver and persists one primary token for the operation.
After `PathComponentRejectedError`, it derives
`<bounded-readable-prefix>.windshare-<six-lowercase-base32>`. The prefix is sanitized and truncated.
The primary RFC 4648 Base32 token is exactly six lowercase characters and is shared by compatible
names in the operation. If that complete candidate is occupied, derive a deterministic six-character
fallback from the operation ID, complete canonical logical path, entry kind, and small attempt index;
persist the selected physical name so retry, reload, and resume cannot change it.

For the exceptional component only:

1. Skip candidates already present in the destination, already claimed by the operation, or already
   known as logical siblings.
2. Try candidates through the ordinary absent/no-replace path up to one named, small retry limit.
3. Persist the selected logical-path/kind-to-physical-component mapping before creating the entry,
   then use the ordinary ownership, handle, checkpoint, and file-transaction rules.
4. If no candidate works, isolate the affected file or subtree when possible. An unusable result
   entry follows the ordinary operation-failure path.

Do not build a rename graph or recursively remap collisions. Progressive discovery can reveal a real
logical entry whose name equals an already committed compatible name. Treat that later entry as a
rare per-item collision, keep the compatible entry, and finish with the ordinary partial result.
This deliberate limitation keeps allocation and restoration linear; the UI and diagnostics must
identify the omitted logical path.

Keep three result-entry names semantically distinct: `requestedName` is the artifact name,
`logicalReservedName` is the ordinary collision-selected no-replace target and restoration target,
and `physicalName` is the compatible FSA component. The canonical reservation records all applicable
root facts before intent freeze; descendant physical names never rewrite the intent.

A small operation ledger in the existing IndexedDB output repository is keyed by exact logical path
and kind. Each row records the selected physical component, ownership/commit state, and, once
committed, one contiguous immutable commit ordinal. Catalog identities, revisions, progress, and
checkpoints remain logical. Load mappings into the session resolver once; every FSA open, verify,
cleanup, recovery, and settlement path uses the selected physical component and verifies persisted
ownership where available. Running the restoration script ends browser resumability for that output;
the browser is not expected to reopen a name that its own FSA policy rejects.

Only an ownership-verified directory or committed file transaction appears in repair counts and in
the restoration sidecar. Ordinary paths bypass the ledger.

### Prepare one simple script and sidecar

Before creating the first compatible target, create one immutable platform-appropriate script and
one adjacent versioned sidecar. Put the pair inside the received directory when the result root keeps
its logical reserved name; put it beside the result entry when the result root itself was replaced.
If pair creation fails, do not create the compatible target. Remove an empty owned pair if no mapping
commits.

The pair uses tokenized, operation-owned names allocated through the same bounded no-replace and
claimed-name checks. It participates in the physical namespace; a later logical collision follows
the same rare per-item partial-result rule.

The script is a fixed template and contains no sender-controlled path literals. The sidecar has only:

1. One header with the format version, operation ID, and output-root placement.
2. Base64-encoded UTF-8 logical relative path, compatible physical component, and entry-kind records,
   appended only after the corresponding directory is ownership-verified or file transaction commits.
3. A checkpoint footer after each published batch containing its cumulative committed record count
   and whether the operation is still active or terminal.

IndexedDB remains runtime truth. A committed mapping only marks the sidecar projector dirty; the
content worker does not await sidecar I/O. One background writer collects every currently
unpublished committed row, appends them as one batch followed by a checkpoint footer, closes the
write, and repeats only if another mapping arrived meanwhile. The last valid footer count is the
publication cursor: it names the committed ordinal prefix already present in the sidecar. Do not
persist a second cursor that would need to commit atomically with filesystem I/O. A dirty flag and
this cursor replace an unbounded per-mapping queue.

Every successfully closed checkpoint is directly readable. Before the first rename, the script finds
the last structurally valid footer, verifies its cumulative count, decodes all records through that
footer, and ignores an incomplete trailing batch. It rejects malformed, duplicate, absolute, or
escaping paths inside the selected checkpoint. The footer is a plain completeness marker, not a
checksum, signature, or mutable recovery journal.

On projector restart, parse the last valid footer, truncate the incomplete tail or rebuild the owned
sidecar, then append ledger rows whose commit ordinal is greater than the footer count. A crash before
or after sidecar close therefore neither skips nor duplicates a committed mapping.

A terminal footer (`completed`, `stopped`, or `failed`) runs without an activity warning. An active
footer remains usable after an abnormal browser exit, but the script first asks the user to confirm
that WindShare is no longer receiving and that the unfinished receive will not be resumed. The UI
never recommends running the script while the operation is receiving or resumably paused.

At completion, failure settlement, or Stop-and-keep-partial, quiesce output mutations, wait for the
background projector, reconcile it with committed ledger rows, and append the terminal footer. A
resumable Pause keeps the latest active checkpoint. Recovery can rebuild the owned sidecar from the
ledger without requesting revisions or blocks.

The script indexes mappings by logical path and processes them deepest-first with the result entry
last. Before each rename it walks from the recorded anchor to the current parent. At every mapped
ancestor it accepts exactly one of the logical or compatible component; both means conflict and
neither means missing output. Unmapped ancestors must exist under their logical names. It then applies
this state table to the leaf inside that resolved parent:

| Compatible source | Original target | Action |
| --- | --- | --- |
| present | absent | Rename with the platform no-replace primitive. |
| absent | present | Treat as already restored. |
| present | present | Stop and report a conflict; never overwrite. |
| absent | absent | Stop and report missing output. |

An interrupted run therefore resumes by executing the same script again. It never overwrites,
deletes, or recursively repairs anything, and it does not delete itself after success. On completion
it reports that rerunning is safe and that the user may delete the script manually.

Each supported script template must use a tested platform primitive that atomically refuses an
existing target for both files and directories. Never emulate no-replace with a separate existence
check followed by an overwriting rename. If no proven template is available for the selected
platform, do not create a compatible target.

The `source absent / target present` case intentionally uses path state rather than proving OS object
identity. This can misclassify deliberate external replacement as an already restored entry, but it
performs no destructive action. Stronger identity proof would require platform-specific metadata or
a per-entry journal and is outside this accepted low-frequency trade-off.

Close and validate the terminal checkpoint before exposing the normal command or publishing a
repaired terminal result. If finalization fails, keep the last complete checkpoint usable, durably
retain a versioned pending outcome and repair summary beside the committed ledger, and offer a local
catch-up retry. The retry only reconciles the sidecar and lifecycle; it never requests revisions,
blocks, or retransmission, and no terminal result claims complete sidecar coverage beforehand.

### Keep the user informed

On the first replacement, show one non-blocking persistent notice such as “The browser rejected an
original name; WindShare is saving a compatible name and prepared a restoration tool.” Update its
count without opening repeated dialogs. While receiving or paused, state that the script becomes
routinely runnable after completion or Stop-and-keep-partial, and is only an abnormal-stop recovery
tool while its latest checkpoint remains active.

After the terminal sidecar checkpoint, retain the ordinary lifecycle (`published`,
`partial-directory`, or the existing failure state) with an immutable repair summary. Every active,
retained, and final result
projection must qualify the result, show the replacement count and a bounded logical-path sample,
and point to the exact script and sidecar with the short platform command that runs it. Do not claim
that the on-disk names are original before restoration.

### Make settlement quiescent

Use one worker-family supervisor for prepared and discovery paths. It latches the first terminal
failure, aborts the producer and queue, awaits the producer and every original worker promise, then
rethrows the initiating failure. An early-rejected `Promise.all` aggregate is not a drain primitive.

Give `PersistentTreeOutputSession` a non-serializing mutation-admission barrier. File and directory
admission enters before its first await; successful file admission remains registered through
transaction close. Final settlement closes external admission and drains the producer, original
workers, admissions, and transactions before draining and reconciling the sidecar projector.

Publishing the terminal sidecar checkpoint is the last materialization mutation. Only after it
completes may settlement seal the materialization cut, emit `settlement.started`, and read checkpoint
and ownership evidence.
Later settlement-owned receipt/lifecycle persistence, checkpoint retirement, cleanup, and authority
release cannot change materialization evidence or replace the initiating failure.

### Preserve evidence

Retain the original FSA stage, raw `TypeError`, pre-mutation cut, entry kind, operation, output
session, protocol generation, logical path, selected physical component, attempt count, and owned
object correlation in local diagnostics. Record script/sidecar identities, the latest checkpointed
mapping count, and active/terminal footer state without exporting sidecar paths. Browser/version and
platform remain best-effort facts only.

## Verification

- Cover file, directory, and result-entry rejection without extension or message matching, and prove
  that the original rejected component caused no mutation or content request.
- Cover stable compatible allocation, occupied-candidate retry, derived-name rejection, later logical
  collision isolation without a rename chain, the three result-entry name roles, reload/resume, and
  logical checkpoint lineage.
- Exercise the four script states, hostile portable names, path confinement, deepest-first ordering,
  nested ancestor rebasing across interruption and rerun, proven platform no-replace behavior, and
  retained script after success.
- Prove the pair exists before the first compatible target, sidecar records include committed
  mappings only, the last complete checkpoint survives an incomplete trailing batch, and an abnormal
  stop can use it after explicit confirmation. Terminal and Stop-and-keep-partial catch up every
  committed ordinal exactly once; finalization retry performs no sender or content work.
- Prove content workers never await the coalescing sidecar projector, concurrent commits do not create
  an unbounded queue, and settlement alone drains the projector.
- Interleave producer, workers, admissions, transactions, finalization, close, and settlement to
  prove complete drain, stable first failure, and no materialization mutation after the cut.
- Prove an ordinary successful receive adds no FSA probe, mapping persistence, restoration artifacts,
  notice, or worker serialization. Update maintained output docs and run focused Web tests, `make web`,
  `make browser`, and `make ci`.

Static extension rules and a general cross-filesystem compatibility framework remain out of scope.
