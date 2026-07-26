# Native filesystem output ownership and resume contract

Status: implemented candidate. Certification is limited to process-restart durability on Linux/ext4 and Windows/local NTFS; no power-loss durability is claimed. Browser output has a separate design.

## Product and durability contract

WindShare resumes verified progress after a process restart and never automatically overwrites or removes a user-visible file.

| Event | Required result |
|---|---|
| Successful receive | The final path appears only after all bytes and file metadata are installed. Successful completion removes all session recovery objects. |
| First interrupt, orderly shutdown, or exhausted network retries | Pause at the latest verified checkpoint. Preserve the original failure for the caller and report that progress was retained. |
| Process crash or forced termination | A later process may use only state and filesystem objects that pass recovery validation. Bytes after the last checkpoint may be retransmitted. |
| Final path already exists | Never replace it. Settle that file as a collision; other files continue. A complete witnessed object is retained when publication is blocked. |
| File ownership is ambiguous | Quarantine only that file namespace. Never modify the final path; other files continue. |
| Matching session header is corrupt | Block that resume-intent namespace. Do not create a competing session for the same intent. |
| Global control metadata is corrupt | Block the output root because session namespaces cannot be classified safely. |
| Output ancestry or its authority no longer matches | Return `Session/NamespaceUnsafe`. Reject a fresh selection before state/content; pause and preserve a matching header/candidate or runtime intent. Never quarantine or root-poison a descendant mismatch. |
| Unsupported platform or filesystem | Fail before creating .windshare-output, any session state, or receiving file content. |
| Runtime output-authority denial | Pause the job at the latest verified checkpoint and retain the witnessed object. The denial alone never authorizes retirement, quarantine, or deletion; retry may publish after authority is restored. |
| Explicit discard | Preview the fixed session object and its no-follow size, then remove only internal recovery objects. Never remove a final path or user directory. |

The initial certified durability level is **process restart only**:

- Linux on ext4;
- Windows on a local NTFS volume.

Process-restart durability covers receiver termination while the kernel and mounted volume remain running. It does not cover OS crashes, reboot, sudden power loss, controller caches, or storage failure. The implementation still uses ordered file and directory synchronization so recovery cuts are explicit, but no power-loss claim may be exposed until each filesystem has passed real power-cut fault testing.

Unknown platforms, non-allowlisted filesystems, network filesystems, FUSE, cloud-placeholder namespaces, Windows remote volumes, reparse-based roots, and nested mounts are rejected. There is no silent non-recoverable fallback.

Recovery state does not expire automatically. The CLI must provide resume list and explicit resume discard. A second interrupt exits immediately and is recovered as a process-crash cut.

## Identity and threat boundary

Persisted inode numbers, device numbers, or Windows File IDs are comparison hints, not ownership proof: identifiers can be reused after object deletion. File-object ownership instead uses:

- a CSPRNG-generated 32-byte OutputObjectID for private names;
- a persistent hard-link anchor that keeps the file object alive;
- same-file comparison of currently open objects;
- a fixed, no-follow private namespace reached only through directory handles.

The anchor prevents deletion of the witnessed object, so its current inode or File ID cannot be reused while resumable state relies on it. A checksum detects accidental state damage but is not a MAC.

OutputRootBinding is a derived digest over the certification ID, current volume identity claim, and fixed output-root directory identity claim. Raw native identity claims are not persisted as authority. The binding is recomputed from the opened root handle before control or session state is trusted, so copied metadata cannot authenticate a different root.

OutputAncestryBinding is the session-header aggregate: a fixed digest stored inside checksummed header.state. It domain-separates and deterministically length-frames OutputRootBinding, the exact canonical SelectionIdentity, and the complete ordered ancestry closure; it never persists a raw claim list. The closure contains the output root, every selected directory, every selected file's final parent, all connecting parents, and the platform-native external proof that the output-root handle still occupies its admitted placement. The same selection at a different ancestry therefore keeps its ResumeIntent but cannot use that session until the original binding validates again.

On Windows, the external component commits to the certified proof result that the same-volume/Object-ID root occupies its canonical path under full-chain no-delete-share pins for each public operation; it persists no raw external-ancestor identities and does not extend selected-closure DACL authorization to ambient ancestors.

Within each platform's certified authority scope, ancestry rejects effective rename/delete-child authority held by an unprivileged principal other than the receiver's OS identity. Privileged root/administrator and a hostile process running as the receiver's own OS identity are outside the threat boundary. Platform-specific ownership and ACL scope is defined under Filesystem certification.

Windows pins the full external drive-root-to-output-root chain without delete sharing during each public operation, then rebinds the root's current volume and Object ID after every unpinned gap; [CreateFile sharing rules](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-createfilew) make the pins effective only while their handles remain open. Linux directory descriptors remain attached to renamed directories but do not prevent rename. Its final validation and `linkat` are not an atomic guard against a hostile same-UID rename/delete race; the [openat rationale](https://man7.org/linux/man-pages/man2/open.2.html) and [rename semantics](https://man7.org/linux/man-pages/man2/renameat.2.html) support no stronger claim.

The contract otherwise covers process restarts, defined namespace crash cuts, identifier reuse, path rename or replacement between runs, non-malicious external collisions, and later ancestry-authority drift involving other unprivileged identities. It does not defend against writes through another hard link to the same object, bit rot, or a filesystem that lies about its behavior. This milestone does not detect in-place content modification.

## Resume intent and admission

Resume identity is scoped to the exact canonical selection, not merely to the share.

CanonicalSelectionV1 contains:

- ShareInstance, SyntheticRoot directory identity, and the terminal root generation;
- selection mode and explicit default-selected value;
- for node-ID mode, directory/file kind, identity, and selected value, sorted by raw node identity;
- for path mode, canonical catalog paths, deduplicated and sorted bytewise;
- the terminal selected directory plan, sorted by canonical output locator and including authenticated directory/generation identity and metadata;
- the terminal selected file plan, sorted by canonical output locator and including FileID, expected size, and authenticated catalog metadata;
- native-tree output mode and the fixed no-replace collision policy.

UI ancestry hints, traversal order, concurrency, progress-display options, and output-root spelling are not selection semantics and are excluded. The output root is bound separately by the filesystem authority.

ResumeIntent is computed only after terminal catalog selection. It is the SHA-256 digest of the domain separator windshare/output-resume-intent/v3 followed by the canonical length-delimited encoding of CanonicalSelectionV1. The same canonical selection must produce identical bytes on every platform. Any selected-entry, locator, identity, size, or catalog-generation change produces a different intent; a later FileRevision remains bound independently by its file record. This uses catalog metadata only and never pre-reads or pre-hashes sender content. The digest is an identifier, not an authenticator.

The namespace key deliberately binds both normalized request semantics and the frozen terminal plan. Implementations may compute separate request and plan digests for streaming, but ResumeIntent must domain-separate and combine both; neither digest alone is the session namespace.

The runtime order is mandatory:

1. Validate and canonicalize the selection request.
2. Discover the selected catalog to terminal success, build the complete OutputSelection, reject duplicate or non-canonical locators and plan/schema-limit violations, and bind CanonicalSelectionV1 and ResumeIntent. Discovery requests neither a file revision nor a file range.
3. Call OutputAuthority.OpenSelection with that terminal, canonically bound OutputSelection. The transfer job performs no output-filesystem preparation before this boundary.
4. Inside OpenSelection, create the output root through a no-follow component walk if requested, acquire its fixed handle, and identify the platform, current volume, and backing filesystem read-only. Only allowlisted Linux/ext4 or Windows/local-NTFS roots may proceed.
5. Before probe or any mutation other than disclosed Windows Object ID enrollment, validate every selected locator and metadata claim through pure platform rules, reject platform-equivalent duplicates, and preflight existing names and parents. Fixed parent and selected-directory handles must pass the certified filesystem, mount, inode-policy, ancestry-authority, and exact-metadata checks; every fresh Windows ancestry handle runs create-or-get before identity comparison. Reject the whole selection when a first component equals, aliases, or begins with an internal fixed, bootstrap, probe, or legacy prefix; the reserved families are .windshare-output and .wsresume-output.
6. Open and retain the native external placement chain, validate its platform rules, and derive OutputRootBinding. Windows pins the full chain and runs `FSCTL_CREATE_OR_GET_OBJECT_ID` on every fresh ancestry handle before comparison; this is the disclosed persistent metadata mutation described under Filesystem certification.
7. Run the backend feature probe in a unique temporary directory. The probe contains no received content and is removed and parent-synced before continuing.
8. Use one bounded temporary inode to prove exact native representation of the maximum logical size and the minimum and maximum modified time at each represented precision (at most seven native witnesses: one size and six times). Then create required selected directories, build the complete ancestry closure, validate its platform-scoped ownership/authority rules, and derive OutputAncestryBinding. Every new Windows handle follows create-or-get-before-compare; retained same-handle checks remain read-only.
9. Only after the whole selection passes admission may OpenSelection revalidate OutputRootBinding and OutputAncestryBinding, bootstrap or validate global control state, and open the one session for ResumeIntent.
10. After OpenSelection returns an OutputSession, every selected-directory FinalizeDirectory operation must revalidate ancestry authority and recompute and match OutputAncestryBinding. For each file, open its revision descriptor and lease and construct OutputFileTarget, then call BeginFile; BeginFile performs the same revalidation before creating file state, and only a transactional FileStart permits file-range requests.
11. Revalidate ancestry authority and recompute and match OutputAncestryBinding again immediately before the final no-replace hard link. Every fresh Windows ancestry handle at these boundaries follows the create-or-get-before-compare rule.

An unsupported root or reserved selected first component is rejected before probe mutation, creating or trusting resume state, file-revision requests, or content requests. A feature-probe or certifiable selected-parent admission failure also precedes creating, trusting, or mutating resume state, revisions, and content. A failed Windows rebind operation may leave only the disclosed invisible Object ID, USN, and last-change metadata; it never creates WindShare state or content.

A fresh frozen-selection or selected-ancestry rejection with no matching header or candidate returns `Session/NamespaceUnsafe` before state, revisions, or content and has no namespace to pause. The same static or physical fault against a matching header/candidate, or an ancestry fault during BeginFile, FinalizeDirectory, recovery, or publication, pauses and preserves that intent's header, candidate, records, stages, anchors, and checkpoints while keeping the namespace exclusive. Restoring the admitted selection semantics or ancestry permits retry. A descendant mismatch stays session-scoped and never poisons the output root or authorizes quarantine, retirement, discard, deletion, or a competing session.

Admission does not claim to predict arbitrary path-scoped MAC/sandbox policy or later authority changes. A policy denial during metadata installation or final linking is a typed file-scoped OutputFault, not an unsupported-filesystem result: pause the job at the latest verified checkpoint and preserve the record, stage, and anchor that witness the object. The denial alone never authorizes retirement, quarantine, discard, or deletion; retry may publish after authority is restored without requesting already verified content.

After ancestry has rebound successfully, admission creates directories with handle-relative no-replace operations and never rolls them back. A later non-rebind failure may therefore leave requested empty user directories, which are reported and never treated as recovery state. Windows Object IDs are likewise never removed or replaced.

## Handle-relative filesystem authority

The initial root path is used only to acquire the output-root handle. Persisted paths are locators, never authority. The native external root-placement proof is reconstructed from handles rather than trusted from the original spelling. All later filesystem inspection, stat, open, create, link, replace, sync, rename, and remove operations are relative to fixed root/control/session/shard/final-parent handles.

Every path walk must:

- stay beneath the fixed output-root handle;
- reject symlinks and Windows reparse points;
- reject mount or volume transitions;
- open each directory component no-follow;
- compare the parent entry with the fixed handle before namespace removal.

Linux uses beneath/no-symlink/no-xdev handle-relative primitives. Each Windows public operation pins the complete external chain from drive root through output root with fresh no-reparse handles opened without `FILE_SHARE_DELETE`; across operation gaps, the next operation must rebind the root's current volume and Object ID before trusting descendants. Every fresh Windows ancestry handle runs `FSCTL_CREATE_OR_GET_OBJECT_ID` before comparison. Only repeated checks on the same retained handle may use its read-only IdentityClaim; claims are never cached across close/reopen boundaries. A string-based fallback that re-resolves an absolute path is not certified.

The final parent handle remains fixed during each publication attempt. Atomic no-replace publication creates a hard link from the anchor handle into that parent. A backend that cannot provide this operation is unsupported.

## Private state and crash-safe bootstrap

The installed layout is:

~~~text
.windshare-output/
├── control.state
├── coordinator.lock
└── sessions/
    └── <resume-intent>/
        └── <session-id>/
            ├── header.state
            ├── session.lock
            ├── files/<shard>/<locator-digest>.state
            ├── anchors/<shard>/<object-id>.anchor
            └── stages/<shard>/<object-id>.stage
~~~

POSIX control directories use mode 0700. Windows uses Hidden plus a protected ACL that does not inherit unnecessary principals. OutputObjectID is 32 random bytes and SessionID is 16 random bytes. ResumeIntent, locator digests, and object IDs use exactly 64 lowercase hexadecimal characters; session IDs use exactly 32; shards use the first digest byte as two lowercase hexadecimal characters. Alternate spellings are invalid. All names are allocated with exclusive create.

The resume-intent directory name routes corruption without trusting header contents. A corrupt header, malformed session entry, duplicate session, or version mismatch blocks only its enclosing intent namespace. A non-canonical spelling that decodes to an intent blocks that intent; a wholly unparseable intent entry is retained and listed as opaque unsafe state without blocking valid sibling intents. Listing represents a malformed or non-canonical fixed session child with an opaque reference bound to its fixed parent and current entry object, so explicit discard can revalidate that exact object without accepting an internal path. A corrupt file record blocks only its locator-digest namespace and is reported as quarantined. Corrupt control.state, coordinator.lock, the sessions directory itself, or control-directory type/root binding blocks the entire output root. A valid matching header or candidate whose freshly derived OutputAncestryBinding differs is not corrupt: `Session/NamespaceUnsafe` pauses and preserves that intent, and a descendant mismatch never blocks the root.

Bootstrap occurs only after filesystem certification and selection admission:

1. Build .windshare-output.bootstrap-<nonce> through a newly fixed directory handle.
2. Install and reopen-verify the control.state envelope, then create and sync coordinator.lock and sessions; sync the bootstrap directory.
3. Atomically install the complete directory at .windshare-output with no replacement, then sync the output root.
4. A concurrent loser validates the installed control object and removes only its own still-fixed candidate.

On restart, an exact partial bootstrap candidate may be completed from the independently derived root binding, and an exact complete candidate may be installed no-replace. A losing candidate is removed only when every entry has the expected type and binding; its sessions child is removed first, coordinator.lock next, and control.state last so every cleanup cut remains classifiable. A malformed or identity-mismatched bootstrap/control candidate is global ambiguity and blocks the root; it is never recursively guessed away.

Session creation uses the same rule inside the matching resume-intent directory: build .candidate-<session-id>, install and verify header.state as its envelope, add session.lock and the three empty data directories, sync and reopen-verify the whole candidate, then atomically install it as <session-id> with no replacement. A partial candidate is completed only when its exact envelope and prefix cut match the current control object, canonical selection, and OutputAncestryBinding. `Session/NamespaceUnsafe` preserves a matching candidate rather than treating it as a losing candidate. Losing-candidate cleanup removes the empty data directories first, session.lock next, and header.state last. A candidate is not a session and cannot receive content until installed.

coordinator.lock is stable for the lifetime of the installed control directory and is never unlinked. This prevents lock-file inode ABA.

## Lock order and namespace removal

The only cross-process lock order is:

~~~text
coordinator.lock -> session.lock -> in-process file mutex
~~~

No code may acquire a lock to the left while holding one to its right.

OpenSelection holds the coordinator lock while it opens the intent/session entries, acquires session.lock, and revalidates the header and fixed session directory. It then releases the coordinator lock and keeps session.lock for the live session.

Completion and discard use a two-step transition to avoid lock inversion:

1. Under session.lock, stop new work, close file transactions, durably write completing or discarding, then release session.lock.
2. Acquire coordinator.lock, reacquire session.lock, reopen and compare the intent/session parent entries with the fixed objects, and revalidate the terminal header.
3. Perform or recover phase-aware cleanup while holding both locks: remove the empty stages, anchors, and files trees, remove session.lock, remove header.state last, then remove the empty session shell before releasing coordinator.lock.

OpenSelection never returns a Completing or Discarding session; it waits, reports busy, or recovers the terminal transition. A crash between the two steps leaves a durable transition that the next authority can finish. For those terminal phases only, each suffix of the cleanup order is a valid cut, including a missing session.lock with the terminal header still present; the exact empty session shell is the final header-absent cut. Listing and discard recognize these cuts instead of fabricating a new lock. Automatic cleanup processes only names declared by valid records; unknown entries are retained and reported.

## Bounded canonical state

All limits are named exported or package constants and are checked before allocation:

| Bound | Value |
|---|---:|
| MaxControlStateBytes | 64 KiB |
| MaxSessionHeaderBytes | 64 KiB |
| MaxFileStateBytes | 1 MiB |
| MaxStateNestingDepth | 16 |
| MaxDurableRangesPerFile | 16,384 |
| MaxFilesPerSession | 1,048,576 |
| MaxSessionsPerIntent | 64 |
| State shard count | 256, selected by the first digest byte |
| Canonical locator bytes/depth | catalog.MaxPathBytes / catalog.MaxPathDepth |

Control, header, and file records use a versioned canonical encoding with a length-delimited envelope and SHA-256 checksum. Decoders use a bounded reader, reject unknown or duplicate fields, reject trailing bytes and non-canonical encodings, validate enum ranges and integer overflow, and validate the checksum before using identities or allocating variable collections.

Only one usable session is legal per intent. MaxSessionsPerIntent is a bounded inspection limit, not permission to create multiple sessions; two valid sessions or any unclassifiable matching sibling make that intent unsafe.

- control.state contains record magic/schema, backend/certification ID, ProcessRestart durability, the derived OutputRootBinding, and control generation;
- header.state contains record magic/schema, backend, root binding, share/synthetic-root identities, ResumeIntent, SelectionIdentity, OutputAncestryBinding, SessionID, session phase/generation, and bounded plan counts;
- each file record contains record magic/schema, SessionID, canonical final locator and LocatorDigest, descriptor identity/exact size/metadata, OutputObjectID, phase, StateGeneration, CheckpointGeneration, canonical non-overlapping durable ranges, and phase-specific reason.

The header never contains the per-file collection or raw ancestry claims. StateGeneration advances on every file-record install; CheckpointGeneration advances only when durable ranges advance and is the generation exposed by VerifiedDurableRanges. LocatorDigest is an index only and must be recomputed from the record locator.

Every initial control, header, or file-record envelope is written to an exclusive candidate temporary, synced, reread, linked into its fixed name with atomic no-replace semantics, parent-synced, and reopen-verified before the temporary is removed and the parent is synced again. Every later state update writes and syncs an exclusive temporary, atomically replaces the private target, parent-syncs, and reopen-verifies the installed target. In-memory durable state advances only after installed-target verification. File checkpoints never rewrite the session header or another file record.

At the ProcessRestart level only, reopening the fixed name with the exact expected generation settles a preceding link/replace or parent-sync syscall report: the same running kernel has proved which generation is currently installed. A structured `state install cut adopted` trace records a stable operation/stage, session/locator context, generation, and which syscall reported failure. Exact reopen never settles a handle-close failure. That cut is Adopted with an error: the observed generation remains authoritative, the current open attempt stops (and any exposed session owner is poisoned), and recovery requires a fresh opener; an active job must pause. This rule is not power-loss evidence.

An update temporary is never authoritative or promoted during recovery. Its name binds the exact fixed target name—the locator digest for sharded file state—to a random 32-byte nonce. After the installed target is validated, recovery may remove only an exact regular temporary for that target and must sync the parent. A malformed file-state temporary quarantines the named locator when its name is parseable; otherwise it leaves the enclosing session in needs-attention without blocking unrelated intents.

## Invariants

- Terminal catalog discovery and canonical selection precede OutputAuthority.OpenSelection. Whole-selection reserved-first-component rejection precedes probe or ancestry-identity mutation; root certification, feature probing, complete-ancestry admission, and every certifiable selected-parent check then precede control bootstrap, creating or trusting resume state, file revisions, and received content.
- Exactly one valid session may own a root plus resume intent.
- A non-empty durable range is trusted only with a valid file record, valid anchor, and a matching current stage, final, or retained open data handle as the phase permits.
- The anchor entry is installed and parent-synced before the first non-empty range can become durable.
- A resumable phase never removes the anchor. Only a durable retiring record revokes range authority and permits ordered link removal.
- Publication is handle-relative, atomic no-replace, and sourced from the anchor. WindShare never unlinks or replaces a final entry.
- A matching final is adopted only from a durable publishing/published history; unexpected matching links are ambiguous.
- A different final is an ordinary collision before publication, a deterministic publishBlocked result only when the current link operation returned already-exists, and ambiguous when merely observed after restarting in publishing.
- OutputRootBinding, platform-scoped ancestry authority, and OutputAncestryBinding are revalidated at OpenSelection, BeginFile, every recovery pass, FinalizeDirectory, and immediately before publication linking; stage, anchor, control state, and every final parent remain on that certified root volume with no nested mount traversal.
- Every fresh Windows ancestry handle runs create-or-get before comparison; only a retained same-handle IdentityClaim is read-only, and no claim survives a close/reopen gap.
- Fresh frozen-selection or selected-ancestry rejection is `Session/NamespaceUnsafe` without a pause when no matching namespace exists. The same fault pauses and preserves a matching header/candidate or runtime intent; descendant mismatch is never root-scoped, file-object ambiguity, quarantine, or cleanup authority.
- Pause preserves the last verified generation. Cancellation and unclassified or retryable transfer failure cannot authorize retire or discard; retire requires a typed permanent isolated reason.
- A path-scoped policy denial during metadata installation or publication produces a typed file-scoped OutputFault. It forces JobPaused through JobPauseOutputFailure with the witnessed object retained and cannot by itself authorize retirement, quarantine, discard, or deletion.
- File retirement follows stage, anchor, record order. Terminal session cleanup is phase-aware and retains its header envelope until every child and session.lock is gone; explicit discard remains separate user authority.
- The cross-process lock order is coordinator, session, then in-process file mutex.

## Durable state machines

### File phases

~~~text
reserved -> witnessed -> publishing -> published -> retiring
                         publishing -> publishBlocked -> publishing

reserved | witnessed | publishBlocked -> retiring
publishing -(invalidated revision, verified missing final)-> retiring
reserved | witnessed | publishing | publishBlocked | published | retiring -> quarantined
~~~

- reserved: the record durably allocates identity and names; no non-empty durable range is legal.
- witnessed: stage and anchor are the same open regular file and the anchor directory entry is synced.
- publishing: complete ranges and metadata are synced and a durable publication attempt is authorized.
- publishBlocked: a different final path prevented no-replace publication; complete stage and anchor remain.
- published: the final and anchor were verified as the same object after syncing the final parent.
- retiring: resumable data authority is revoked and only ordered internal cleanup is legal. The record includes Published, IsolatedFailure, PreObjectCollision, or InvalidatedRevision as its retirement reason.
- quarantined: no automatic resume, publication, or cleanup is legal.

publishBlocked is an ordinary recoverable collision; quarantined means ownership or history is ambiguous.

An authenticated revision-only mismatch never reuses old ranges. A reserved or witnessed record may retire its internal witness after recovery verifies that the final is missing or safely different; a safely different final remains untouched. For a publishing record, a matching final is adopted as published before internal retirement, a different or unsafe final quarantines, and only a verified missing-final cut may enter invalidated-revision retirement directly. A published record is revalidated before retirement, with mismatch or unsafe evidence quarantined. A publishBlocked record may retire its internal witness while preserving the known different final. Any already durable invalidated retirement finishes stage, anchor, then record cleanup before a new object is allocated. The current revision then follows ordinary no-replace collision rules.

### Session phases

~~~text
active -> pausing -> paused -> active
          pausing -> paused-needs-attention -> active
active -> completing -> absent
active -> completing -> paused-needs-attention
active | paused | paused-needs-attention -> discarding -> absent
~~~

absent is not persisted. Per-file quarantine does not make the header corrupt. A valid session with blocked or quarantined files remains paused-needs-attention.

## Core API settlement contract

Abort and AbortJob are removed. The breaking core API expresses caller intent and returns typed settlements:

~~~go
type OutputAuthority interface {
    OpenSelection(context.Context, OutputSelection) (OutputSession, error)
}

type FileTransaction interface {
    Binding() OutputFileBinding
    WriteRange(context.Context, uint64, []byte) error
    Checkpoint(context.Context) (VerifiedDurableRanges, error)
    Commit(context.Context) (FileSettlement, error)
    Pause(context.Context, FilePauseReason) (FileSettlement, error)
    Retire(context.Context, FileRetireReason) (FileSettlement, error)
}

type OutputSession interface {
    BackendID() OutputBackendID
    SessionID() OutputSessionID
    Capabilities() OutputCapabilities
    FinalizeDirectory(context.Context, OutputDirectory) error
    BeginFile(context.Context, OutputFile) (FileStart, error)
    PauseJob(context.Context, JobPauseReason) (JobSettlement, error)
    CompleteJob(context.Context, JobOutcome) (JobSettlement, error)
}
~~~

OutputFileTarget is the immutable requested authority: backend, session, revision descriptor, and canonical locator. It exists before WindShare owns an output object, so a pre-object Collision carries a target without inventing object identity. OutputFileBinding adds the concrete OutputObjectIdentity after the backend binds that identity to the target. A transactional FileStart yields this binding and keys all checkpoints and transaction settlements; immediate Retired may carry it only while deterministically finishing the already durable retirement record.

FileStart is a checked sum type: it contains either a non-nil transaction plus its VerifiedDurableRanges, or one immediate FileSettlement. The immediate form covers a pre-state Collision and recovered Published, PublishBlocked, Quarantined, or Retired state. Immediate FileRetired is legal only when BeginFile deterministically finishes an already durable retiring record; it never grants new retirement authority.

FileSettlementKind is exactly Published, Paused, Retired, Collision, PublishBlocked, or Quarantined. Collision has no recovery record; PublishBlocked carries a complete verified binding/range set; Paused carries the last verified checkpoint; Quarantined carries a stable reason code and state reference. Valid method results are:

| Method | Allowed settlements |
|---|---|
| BeginFile immediate | Published, Retired, Collision, PublishBlocked, Quarantined |
| Commit | Published, PublishBlocked, Quarantined |
| Pause | Paused, Quarantined |
| Retire | Retired, Quarantined |
| PauseJob | JobPaused, JobPausedNeedsAttention |
| CompleteJob | JobClosed, JobPausedNeedsAttention |

FilePauseReason and JobPauseReason use stable Interrupted, Shutdown, TransportFailure, SessionFailure, and OutputFailure classes. FileRetireReason uses only IsolatedPermanentSourceFailure, InvalidatedRevision, or ExplicitPolicySkip. Zero/unknown reasons are contract violations; cancellation and retryable failures are never retirement reasons. Raw errors remain in the job result; structured traces expose only typed failure scope and code.

Normal collision, quarantine, and needs-attention outcomes are settlements, not errors. A non-nil error means the requested settlement itself could not be installed or verified. Such errors implement a typed OutputFault with File, Session, or Root scope and a stable code; callers never parse error text or infer cleanup authority from cancellation.

User interruption, orderly shutdown, transport/session failure, and exhausted retries call Pause on active files with a non-cancelled settle context, then PauseJob. A failure returned by terminal Commit has already closed that transaction after preserving its latest verified checkpoint; it still forces PauseJob with JobPauseOutputFailure. A permanently failed and isolated file may call Retire only while ownership is valid. Ordinary transfer or output failures never call discard.

Filesystem path policy lives in the concrete OutputAuthority rather than in TransferJob:

~~~go
NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig) (*FilesystemOutputAuthority, error)
ListResumeState(context.Context, FilesystemResumeRoot) (*ResumeStateInventory, error)
(*ResumeStateInventory).Summaries() []ResumeStateSummary
(*ResumeStateInventory).Close() error
DiscardResumeState(context.Context, ResumeStateRef) (DiscardSettlement, error)
~~~

FilesystemOutputAuthorityConfig owns the root path and create-root policy. Its OpenSelection receives only the terminal OutputSelection already bound to CanonicalSelectionV1, performs certification and whole-selection admission, and returns an OutputSession only after the native session is ready. ResumeStateInventory owns the fixed native entry pins for one inventory and must be closed. Each ResumeStateRef is only an opaque item ID bound to that live inventory; Discard consumes it exactly once and never accepts an arbitrary or serialized internal path. DiscardSettlement reports Discarded or AlreadyAbsent and the removed internal byte count.

## File protocol

Every FinalizeDirectory operation for a selected directory opens a fresh ancestry scope, applies the platform authority rules, and matches OutputAncestryBinding before metadata mutation. On Windows, each fresh handle runs create-or-get before comparison and the full external chain remains pinned for the operation.

### Begin

1. Validate OutputFileTarget against the admitted selection, open a fresh ancestry scope, revalidate platform authority, recompute OutputAncestryBinding, and reopen and validate the final parent. Windows runs create-or-get on each fresh handle before comparison. `Session/NamespaceUnsafe` pauses the matching intent without creating file state. If final already exists and no matching record exists, return Collision with only that target and without creating state.
2. Persist reserved with a fresh OutputObjectID and canonical locator; OutputFileBinding is derived from that record only when the phase-specific ownership proof permits it.
3. Exclusive-create stage, set exact size, sync the file, and sync the stage parent.
4. Hard-link stage to anchor with no replacement and sync the anchor parent.
5. Open stage and anchor and verify same regular file, exact size, expected volume, and record names.
6. Persist witnessed. Only this phase may acquire non-empty durable ranges.

### Checkpoint

1. Merge pending ranges without exceeding MaxDurableRangesPerFile.
2. Sync the open data object.
3. Verify the open object against the anchor handle.
4. Install and reopen-verify the next StateGeneration and CheckpointGeneration of the file record.

The previous generation remains authoritative on any failure.

### Publish

1. Require complete ranges. Set all file metadata through the open object handle and sync it.
2. Verify stage, anchor, and the open object.
3. Persist publishing.
4. Immediately before publication, open a fresh ancestry scope, revalidate platform authority, recompute OutputAncestryBinding, revalidate the final parent, and create the final hard link from anchor with atomic no-replace semantics. Windows runs create-or-get before every fresh-handle comparison. `Session/NamespaceUnsafe` pauses with publishing and all witnesses intact.
5. If this no-replace operation returns already-exists, open the final: a matching anchor continues as an authorized publish cut, a safely different object persists publishBlocked, and an unsafe observation quarantines.
6. On link success, sync the final parent.
7. Verify final and anchor as the same object with expected metadata, then persist published.
8. Remove stage and sync the stage parent. Retain anchor until retirement.

A metadata or final-link policy denial before a final appears is not ambiguous ownership and is not PublishBlocked. Commit returns a typed file-scoped OutputFault and the job settles as JobPaused with the latest checkpoint and witnessed object intact. Once authority is restored, retry may finish publication without receiving verified content again.

A matching final is automatically adopted only when publishing was already durable. Without that marker, the matching link has ambiguous provenance. A direct already-exists result for a safely different final deterministically installs publishBlocked, but after restart publishing plus a different final is ambiguous because a successfully linked final could have been replaced before observation.

## Recovery matrix

All rows first require a valid global control object, intent-scoped header, file record, session lock, root binding, freshly validated platform ancestry authority, a matched OutputAncestryBinding, and a handle-relative namespace walk. Every reducer pass opens a fresh ancestry scope first; Windows runs create-or-get before each fresh-handle comparison. Recovery performs the smallest ordered mutation, syncs and re-observes, then reduces again; it never shortcuts across a durable phase.

| Phase | Observation | Recovery |
|---|---|---|
| reserved | Stage, anchor, and final absent | Repeat object creation. |
| reserved | Stage and anchor absent; a final is safely present | Return ordinary Collision, persist retiring, then remove only the empty record. |
| reserved | Exactly one of stage or anchor exists | Quarantine: an asymmetric restart observation cannot prove continuity. |
| reserved | Stage and anchor match | Reopen-verify and persist witnessed. |
| reserved | Existing internal entries mismatch | Quarantine; remove nothing. |
| witnessed | Anchor and stage match; final absent or different | Resume verified ranges. A different final remains a collision candidate. |
| witnessed | Anchor or stage is missing/mismatched | Quarantine because durable ranges lost their two-link witness. |
| witnessed | Final matches anchor | Quarantine: no durable publication attempt explains the link. |
| publishing | Anchor and stage match; final absent | Retry no-replace publication. |
| publishing | Final matches anchor, metadata/ranges are complete, and stage is absent or matching | Deterministic publish cut: sync final parent, finish metadata verification, persist published, then re-reduce. |
| publishing | Final/anchor match but an existing stage mismatches | Quarantine; preserve the unexpected stage. |
| publishing | A different final exists after restart | Quarantine: collision-before-link and publish-then-replacement are indistinguishable. |
| publishing | Anchor is invalid, or stage is missing/mismatched while final does not match anchor | Quarantine. |
| publishBlocked | Anchor and stage match; different final remains | Keep blocked and report PublishBlocked. |
| publishBlocked | Anchor and stage match; final is absent | Persist publishing and retry without receiving content. |
| publishBlocked | Final matches anchor | Quarantine: blocked state does not authorize adoption of an externally created link. |
| publishBlocked | Anchor or stage is missing/mismatched | Quarantine. |
| published | Final and anchor match; metadata matches; stage matches | Remove stage, sync its parent, re-observe, then report published. |
| published | Final and anchor match; metadata matches; stage is absent | Sync the stage parent, re-observe, then report published. |
| published | Final is absent, differs from anchor, cannot be compared with a verified anchor, or has mismatched/unreadable metadata | Quarantine for final-path ambiguity; do not recreate, modify, or remove final. |
| published | Final/anchor and metadata match but stage cleanup is mismatched, inaccessible, racing, or otherwise ambiguous | Preserve published state and both witnesses, pause for attention, and remove nothing. |
| retiring | Matching stage and anchor exist | Remove stage and sync its parent; continue. |
| retiring | Stage absent; matching anchor exists | Sync the stage parent, remove anchor, sync the anchor parent, then continue. |
| retiring | Stage and anchor absent | Sync the stage and anchor parents again, then remove the retiring record and sync its parent. |
| retiring | Stage exists while anchor is absent, or stage/anchor identity or access is ambiguous | Preserve retiring state and remaining witnesses, pause for attention, and remove nothing further. |
| quarantined | Any observation | Preserve it for explicit discard. |

Only retiring treats both missing internal links as an idempotent cleanup cut. No resumable phase may trust non-empty ranges without the anchor.

A pure operation denial with no ambiguous namespace observation follows the runtime-denial pause rule above. Changed ancestry identity, external placement, or platform-scoped authority is `Session/NamespaceUnsafe`: preserve the matching intent and never quarantine or root-poison a descendant mismatch. Before verified publication, unexplained internal file-namespace ambiguity is quarantined at the narrowest namespace that can be identified safely. After a matching final has been verified and published is durable, only final-path mismatch or ambiguity can quarantine that file; internal cleanup ambiguity preserves the published/retiring record and witnesses and pauses for attention. If valid metadata cannot scope ambiguity to a file or intent, it blocks the output root. Recovery never guesses.

## Pause, completion, retirement, and discard

Pause stops frame admission and new transactions, persists pausing, then uses a non-cancelled bounded settle context. Each active file either installs a data-sync-first checkpoint or retains its previous verified generation. The session persists paused, closes handles, and releases session.lock. Pause never enters retiring and never removes internal data links.

Go cannot reliably cancel a filesystem sync already in progress. The settle deadline prevents new work; a second signal terminates immediately.

Completion re-runs the published reducer before revoking each published witness: the final and metadata must still form a deterministic published cut, and any remaining stage must match before automatic removal. A missing, replaced, unprovable, or metadata-drifted final is quarantined with its internal witnesses preserved. Internal cleanup ambiguity instead leaves published state intact and pauses for attention. Only then does completion retire every verified published record and every explicitly isolated permanent failure. The cleanup order is mandatory:

1. Persist retiring and reopen-verify it.
2. Remove stage first and sync the stage parent.
3. Reverify, then remove anchor as the last internal data-bearing link and sync the anchor parent.
4. Remove the retiring file record and sync its parent.

The state record therefore always survives long enough to find any remaining data-bearing link. Once retiring is durable, it contains no resumable range authority: a missing stage is accepted while the anchor remains, and both missing links are an idempotent post-removal cut. A mismatched or ambiguous internal entry is never removed or quarantined automatically; the retiring record and remaining witnesses stay intact for attention.

After all retireable records are gone, CompleteJob uses the terminal reducer: remove and sync the empty stages, anchors, and files trees, remove and sync session.lock, remove and sync header.state last, then remove and sync the empty session and intent shells. Before the header is removed, each step revalidates the pinned parents and terminal header; afterward only the exact pinned empty shell is removable. Only a Completing header authorizes the preceding missing-prefix cleanup cuts. If blocked or quarantined records remain, completion instead persists paused-needs-attention and returns that non-fatal settlement. Final files and user directories are never completion targets.

Discard is authorized only by an explicit user operation against a ResumeStateRef obtained from listing. It previews the fixed namespace, no-follow entry count, and allocated bytes. With a valid header it persists Discarding and uses the same phase-aware terminal reducer, removing only entries beneath the fixed session handle without following links or crossing mounts. It removes stages before anchors, syncs their parents, removes file records and child trees, removes session.lock, then removes header.state last. A valid terminal header plus a missing lock is therefore a recoverable discard cut. For a corrupt header, the user authorizes the currently fixed session directory object; cleanup pins and revalidates its parent entry before every cut and retains the corrupt envelope until all other authorized internal objects are gone. Any parent-entry replacement aborts the operation.

## Filesystem certification

The recoverable backend requires:

- regular-file hard links and handle-relative atomic no-replace linking;
- current-object same-file comparison;
- exclusive create and handle-relative atomic record replacement;
- file sync, directory sync, and reopen verification;
- root/control/final-parent same-volume and no-cross-mount validation;
- reuse-resistant native directory identities, a native external root-placement proof, and deterministic complete-ancestry binding;
- platform-scoped rejection of non-excluded unprivileged rename/delete-child authority, plus the certified platform's validation-to-use guard semantics;
- process-kill fault tests on the exact allowlisted platform/filesystem pair.

An API feature probe is necessary but not sufficient for certification. It runs only after read-only filesystem identification has matched the allowlist and the complete selection has passed pure locator/metadata validation, platform-key alias checks, actual-name and parent-shape inspection, and certifiable ancestry-authority validation. Windows create-or-get enrollment is the only permitted pre-probe identity mutation. These checks cover the allowlisted filesystem and mount, inode policy, namespace shape, DAC/ACL, and metadata representation; they do not certify arbitrary path-scoped MAC/sandbox policy or later authority changes. Probe leftovers have a strict temporary schema; a valid fixed leftover can be removed, while a malformed reserved probe blocks bootstrap rather than being guessed away.

Probe serialization is itself root-bound authority. Linux locks the already fixed root handle. Windows uses a protected `Global\WindShare.OutputProbe.<sha256>` kernel mutex whose name is derived from the current OutputRootBinding. An ordinary user must be able to create and verify its exact user-plus-SYSTEM security envelope; it serializes processes, and an abandoned owner is recovered by the next waiter. No probe-lock filesystem entry exists before certification or remains after probing.

Linux/ext4 identity is the filesystem/device identity, inode number, and a nonzero `i_generation`; device plus inode alone is never accepted. The ext4 [inode layout](https://docs.kernel.org/filesystems/ext4/inodes.html) defines `i_generation` as the file version, while the ext4 [ioctl contract](https://docs.kernel.org/admin-guide/ext4.html#ioctls) exposes both get-version and set-version operations. Certification therefore requires `metadata_csum`, proves that the receiver cannot spoof the value through `FS_IOC_SETVERSION`, and rejects a zero generation, legacy ext4, or any kernel where that spoof lock cannot be established.

Linux external placement ancestors above the output root may be owned only by UID 0 or the effective receiver UID; the output root and every in-root selected-closure directory must be receiver-owned. Certification rejects an ambiguous POSIX ACL, any group/other write-plus-search (`W+X`) authority, and every shared sticky ancestor including `/tmp`. Ext4 directories also pass the existing casefold, fscrypt, project-inherit, immutable, and append-only checks. A file or directory that needs nanoseconds or seconds outside the signed 32-bit range must expose `STATX_BTIME` on its exact handle as a conservative proof that the ext4 inode layout contains the required extra timestamp fields; probe mutations are synced before exact comparison. Final validation plus `linkat` is not claimed to be atomic against a hostile same-UID process.

Windows/local-NTFS obtains ancestry identity **only** with [`FSCTL_CREATE_OR_GET_OBJECT_ID`](https://learn.microsoft.com/en-us/windows/win32/api/winioctl/ni-winioctl-fsctl_create_or_get_object_id). Every fresh ancestry handle at OpenSelection, BeginFile, recovery, publication, and FinalizeDirectory runs that operation before comparison, reuses any existing Object ID, and binds the result to the current volume because Object IDs are unique only within a volume. Only repeated comparisons while retaining that same handle may use its read-only IdentityClaim. WindShare never calls `FSCTL_GET_OBJECT_ID`, caches a raw claim across handles, calls `FSCTL_SET_OBJECT_ID`, calls `FSCTL_SET_OBJECT_ID_EXTENDED`, calls `FSCTL_DELETE_OBJECT_ID`, or otherwise replaces or removes an Object ID.

Windows owner/DACL certification applies only from the output root downward through the selected closure: it rejects a non-excluded unprivileged owner or effective `DELETE`, `FILE_DELETE_CHILD`, `WRITE_DAC`, or `WRITE_OWNER` authority. External drive-root-to-output-root placement is instead protected per public operation by full-chain no-share-delete pins; after any unpinned gap, the next operation rebinds the root's current volume and Object ID before trusting descendants. Object IDs are persistent NTFS metadata invisible to most applications; a failed rebind can leave only a newly created Object ID and its documented metadata effects, never WindShare state or content. The normative [Microsoft filesystem behavior](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-fsa/4f329bbc-eacf-4839-9e46-dd7780759132) specifies `USN_REASON_OBJECT_ID_CHANGE` and a last-change-time update when an ID is created.

Initial certification tests only:

| Platform | Filesystem | Durability claim |
|---|---|---|
| Linux | ext4 | Process restart |
| Windows | local NTFS | Process restart |

macOS, other Linux filesystems, ReFS/FAT/exFAT, SMB, FUSE, and cloud-placeholder filesystems are not certified. Power-loss tests may be added later as a separate durability level and allowlist entry.

## Schema v2 disposition

Schema v3 uses the private control layout above and never trusts or upgrades v2 device/inode or File-ID records.

- V3 OpenSelection does not adopt, rename, delete, or derive ranges from a v2 journal.
- V2 state does not block an otherwise valid v3 intent because v2 cannot prove the canonical-selection namespace.
- Resume inventory reports v2 as legacy-untrusted even when the filesystem is not certified for v3.
- Explicit legacy discard on a certified root may consume a live inventory item and remove only the fixed v2 journal/control record after the inventory's live root handle, exact entry pin, length, and SHA-256 are revalidated. Listing never creates a persistent NTFS Object ID; a clean root remains unchanged, and a legacy item that cannot retain both live pins is attention-only. Discard does not delete a legacy stage or final whose ownership cannot be proved.
- A conflicting legacy stage is surfaced for manual handling; there is no filename-, age-, or identifier-based automatic garbage collection.

## Observability and validation

Structured traces correlate operations with the stable resume intent, session, locator digest, bound output object, selection identity, ancestry digest, and filesystem certification that apply. A final typed file settlement is emitted exactly once at its semantic API boundary: BeginFile, Commit, Pause, internal job pause, BeginFile cleanup, or Retire. It includes the pause/retire or quarantine reason and typed failure scope/code when applicable. Recovery-decision traces include the selected quarantine reason.

Native-lock traces cover only coordinator and session locks in configured-authority runtime workflows. They report Acquired, Contended, AcquireFailed, Released, or ReleaseReportedFailure; contention is an ownership outcome, not a failure. The List/Discard free APIs use the same internal lock coordinator but accept no tracer, so they promise no observable lock events. Tracers must be concurrency-safe. No trace contains capability secrets, raw errors, paths, lock names, or raw native identity claims.

Required tests include:

- every state install, file/directory sync, hard-link, unlink, and namespace-removal process-crash cut;
- deterministic recovery for bootstrap, Begin, checkpoint, publish, and retiring;
- all recovery-matrix rows, including stage/anchor/final loss or replacement;
- cancellation and network failures settling only as Pause;
- collision isolation, retry after the conflicting final is moved, and no content retransmission for publishBlocked;
- matching-link adoption only from publishing and quarantine of ambiguous matching links;
- corrupt file/header/global metadata at their specified scopes, plus opaque fixed-child references and parent-replacement rejection during discard;
- two OpenSelection calls, OpenSelection versus completion/discard, lock-order assertions, and lock-file ABA;
- whole-selection internal-prefix and platform-alias rejection before probe mutation, including selected descendants beneath a reserved first component;
- deterministic, length-framed OutputAncestryBinding over OutputRootBinding, SelectionIdentity, the root, selected directories/final parents and their closure, and native external root placement, with ordering, duplicate, omission, selection-scope, checksum, and bound tests;
- ancestry revalidation at OpenSelection, BeginFile, every recovery pass, FinalizeDirectory, and immediately pre-link;
- `Session/NamespaceUnsafe` rejection without Pause or state/content for fresh frozen-selection and selected-ancestry failures, pause/preservation for the same failures against a matching header/candidate or runtime intent, restored-selection/placement resume, and proof that a descendant mismatch never root-poisons, quarantines, or creates a competing session;
- pure validation of every selected size/time claim, with native probe call counts bounded to the maximum size and per-precision time extrema (at most seven witnesses);
- ordinary-user Windows mutex creation, exact security-envelope validation, cross-process exclusion, and abandoned-owner recovery;
- Linux/ext4 rejection of zero or changed `i_generation`, device-plus-inode-only identity, successful `FS_IOC_SETVERSION` spoofing, external ownership other than UID 0/receiver, non-receiver-owned in-root closure, ambiguous ACL, group/other `W+X`, and shared sticky ancestry including `/tmp`;
- Windows create-or-get-before-compare on every fresh handle at every required boundary, existing-ID reuse and current-volume binding, read-only same-handle IdentityClaim checks, and proof of no raw cross-handle cache or GET/SET/DELETE call;
- Windows root-down-only owner/DACL exclusion for `DELETE`, `FILE_DELETE_CHILD`, `WRITE_DAC`, and `WRITE_OWNER`, external full-chain no-share-delete pins, root volume/Object-ID rebind after gaps, and failed-rebind Object-ID/USN/last-change-only side effects;
- handle/path replacement, symlink/reparse traversal, nested mount, cross-volume parent, and unsupported filesystem rejection before state, file revisions, or content, plus explicit proof that Linux final validation plus link is not atomic against a hostile same-UID race;
- metadata and final-link policy denial after checkpoint returning a typed file-scoped OutputFault, forcing JobPaused, retaining witnessed state, and publishing on retry without retransmission once authority is restored;
- non-skippable real process-kill recovery on Linux/ext4 and Windows/local NTFS, using a receiver-owned workspace/home ancestry with no group/other write authority;
- successful completion/discard leaving no session object, with only valid fixed global control metadata remaining.

No power-loss or reboot test may be described as passing the product contract until that durability level is separately certified.

## Release sequence

This is a breaking pre-v1 core change and is released core-first:

1. Implement the transfer settlement API, selection/admission boundary, state machines, and Linux/Windows filesystem adapters in the independent core module.
2. Create the exact-SHA candidate tag and pass both commit-bound extracted-module release jobs, including Linux/ext4 and Windows/NTFS native certification, from fresh caches with `GOWORK=off`.
3. Publish the next core module version from that certified commit and verify its origin and sums through the public Go proxy.
4. Update the root module to that published version and integrate root consumers and CLI behavior.
5. Run make ci.

Do not add a replace directive, compatibility shim, Abort adapter, or dual v2/v3 write path.

## Implementation references

- core/transfer/output.go
- core/transfer/job.go
- core/transfer/output_selection.go
- core/transfer/resume_intent.go
- core/osfs/output_authority.go
- core/osfs/output_v3_admission.go
- core/osfs/output_v3_file.go
- core/osfs/output_v3_terminal.go
- core/osfs/internal/resumestate/
- Linux linkat/renameat2/openat2, ext4 `i_generation`, and directory fsync
- Windows `FSCTL_CREATE_OR_GET_OBJECT_ID`, scoped directory guards, and handle-relative file link/rename information
