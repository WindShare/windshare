package outputruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"slices"
	"strings"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func listLegacyResumeState(ctx context.Context, rootPath string) ([]ResumeStateSummary, error) {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	names, readErr := collectLegacyResumeNames(ctx, directory)
	closeErr := directory.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	slices.Sort(names)
	result := make([]ResumeStateSummary, 0)
	for _, name := range names {
		info, err := root.Lstat(name)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			result = append(result, legacyUnreadableSummary(rootPath, name, "legacy-v2-entry-unreadable", err))
			continue
		}
		summary, include := summarizeLegacyResumeEntry(root, rootPath, name, info)
		if !include {
			continue
		}
		result = append(result, summary)
	}
	return result, nil
}

const legacyDirectoryReadBatch = 256

func collectLegacyResumeNames(ctx context.Context, directory *os.File) ([]string, error) {
	names := make([]string, 0)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch, readErr := directory.Readdirnames(legacyDirectoryReadBatch)
		for _, name := range batch {
			if !strings.HasPrefix(name, legacyOutputStatePrefix) {
				continue
			}
			names = append(names, name)
			if len(names) > outputnamespace.RootInspectionLimit {
				return nil, outputfault.ErrInspectionLimit
			}
		}
		if errors.Is(readErr, io.EOF) {
			return names, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

func legacyUnreadableSummary(rootPath, name, code string, cause error) ResumeStateSummary {
	return ResumeStateSummary{
		Reference: ResumeStateRef{
			rootPath: rootPath, kind: ResumeStateLegacyUntrusted, legacyName: name,
		},
		Attention: []ResumeAttention{{
			Scope: ResumeAttentionLegacy, Code: code, State: name, Detail: cause.Error(),
		}},
	}
}

func summarizeLegacyResumeEntry(
	root *os.Root,
	rootPath string,
	name string,
	info fs.FileInfo,
) (ResumeStateSummary, bool) {
	base := ResumeStateSummary{
		Reference: ResumeStateRef{
			rootPath: rootPath, kind: ResumeStateLegacyUntrusted, legacyName: name,
		},
		Attention: []ResumeAttention{{
			Scope: ResumeAttentionLegacy, Code: "legacy-v2-untrusted", State: name,
		}},
	}
	if strings.HasPrefix(name, legacyOutputStagePrefix) {
		base.Attention = []ResumeAttention{{
			Scope: ResumeAttentionLegacy, Code: "legacy-v2-stage-manual", State: name,
		}}
		return base, true
	}
	if !strings.HasSuffix(name, legacyOutputJournalSuffix) {
		return ResumeStateSummary{}, false
	}
	if !info.Mode().IsRegular() {
		base.Attention = []ResumeAttention{{
			Scope: ResumeAttentionLegacy, Code: "legacy-v2-journal-unsafe", State: name,
		}}
		return base, true
	}
	inspectLegacyJournal(root, name, &base)
	return base, true
}

func inspectLegacyJournal(root *os.Root, name string, summary *ResumeStateSummary) {
	file, openErr := root.Open(name)
	if openErr == nil {
		openedInfo, statErr := file.Stat()
		digest, size, digestErr := digestLegacyOutputJournal(file)
		closeErr := file.Close()
		if statErr == nil && openedInfo.Mode().IsRegular() && openedInfo.Size() >= 0 &&
			uint64(openedInfo.Size()) == size && digestErr == nil && closeErr == nil {
			summary.Reference.legacyRemovable = true
			summary.Reference.legacySize = size
			summary.Reference.legacyDigest = digest
			return
		}
		openErr = errors.Join(statErr, digestErr, closeErr)
	}
	if openErr != nil {
		summary.Attention = append(summary.Attention, ResumeAttention{
			Scope: ResumeAttentionLegacy, Code: "legacy-v2-journal-unreadable", State: name,
			Detail: openErr.Error(),
		})
	}
}

func listGuardedLegacyResumeState(
	ctx context.Context,
	rootPath string,
	root outputcap.Directory,
) ([]ResumeStateSummary, error) {
	if root == nil {
		return nil, outputcap.ErrUnsafeNamespace
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	names, err := root.NamesWithPrefix(legacyOutputStatePrefix, outputnamespace.RootInspectionLimit+1)
	if err != nil {
		return nil, err
	}
	if len(names) > outputnamespace.RootInspectionLimit {
		return nil, outputfault.ErrInspectionLimit
	}
	slices.Sort(names)
	result := make([]ResumeStateSummary, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		kind, exact, inspectErr := root.ClassifyExactEntry(name)
		if kind == outputcap.EntryAbsent && inspectErr == nil {
			continue
		}
		if inspectErr != nil || !exact {
			result = append(result, ResumeStateSummary{
				Reference: ResumeStateRef{
					rootPath: rootPath, kind: ResumeStateLegacyUntrusted, legacyName: name,
				},
				Attention: []ResumeAttention{{
					Scope: ResumeAttentionLegacy, Code: "legacy-v2-entry-unreadable",
					State: name, Detail: errors.Join(outputcap.ErrUnsafeNamespace, inspectErr).Error(),
				}},
			})
			continue
		}
		if strings.HasPrefix(name, legacyOutputStagePrefix) {
			result = append(result, ResumeStateSummary{
				Reference: ResumeStateRef{
					rootPath: rootPath, kind: ResumeStateLegacyUntrusted, legacyName: name,
				},
				Attention: []ResumeAttention{{
					Scope: ResumeAttentionLegacy, Code: "legacy-v2-stage-manual", State: name,
				}},
			})
			continue
		}
		if !strings.HasSuffix(name, legacyOutputJournalSuffix) {
			continue
		}
		if kind != outputcap.EntryRegularFile {
			result = append(result, ResumeStateSummary{
				Reference: ResumeStateRef{
					rootPath: rootPath, kind: ResumeStateLegacyUntrusted, legacyName: name,
				},
				Attention: []ResumeAttention{{
					Scope: ResumeAttentionLegacy, Code: "legacy-v2-journal-unsafe", State: name,
				}},
			})
			continue
		}

		summary := ResumeStateSummary{
			Reference: ResumeStateRef{
				rootPath: rootPath, kind: ResumeStateLegacyUntrusted, legacyName: name,
			},
			Attention: []ResumeAttention{{
				Scope: ResumeAttentionLegacy, Code: "legacy-v2-untrusted", State: name,
			}},
		}
		file, openErr := root.OpenFile(name, false, false)
		if openErr == nil {
			digest, size, digestErr := digestLegacyOutputV3Journal(file)
			closeErr := file.Close()
			if digestErr == nil && closeErr == nil {
				summary.Reference.legacyRemovable = true
				summary.Reference.legacySize = size
				summary.Reference.legacyDigest = digest
			} else {
				openErr = errors.Join(digestErr, closeErr)
			}
		}
		if openErr != nil {
			summary.Attention = append(summary.Attention, ResumeAttention{
				Scope: ResumeAttentionLegacy, Code: "legacy-v2-journal-unreadable", State: name,
				Detail: openErr.Error(),
			})
		}
		result = append(result, summary)
	}
	return result, nil
}

func digestLegacyOutputV3Journal(file outputcap.File) ([sha256.Size]byte, uint64, error) {
	if file == nil {
		return [sha256.Size]byte{}, 0, outputcap.ErrUnsafeNamespace
	}
	size, err := file.Size()
	if err != nil {
		return [sha256.Size]byte{}, 0, err
	}
	if size > maxLegacyOutputJournalBytes {
		return [sha256.Size]byte{}, 0, outputfault.ErrLegacyState
	}
	digest, readSize, err := digestLegacyOutputJournal(
		io.NewSectionReader(file, 0, maxLegacyOutputJournalBytes+1),
	)
	if err != nil || readSize != size {
		return [sha256.Size]byte{}, 0, errors.Join(io.ErrUnexpectedEOF, err)
	}
	return digest, size, nil
}

func digestLegacyOutputJournal(reader io.Reader) ([sha256.Size]byte, uint64, error) {
	digest := sha256.New()
	size, err := io.Copy(digest, io.LimitReader(reader, maxLegacyOutputJournalBytes+1))
	if err != nil {
		return [sha256.Size]byte{}, 0, err
	}
	if size > maxLegacyOutputJournalBytes {
		return [sha256.Size]byte{}, 0, outputfault.ErrLegacyState
	}
	var result [sha256.Size]byte
	copy(result[:], digest.Sum(nil))
	return result, uint64(size), nil
}

func attachLegacyResumePins(root outputcap.Directory, summaries []ResumeStateSummary) error {
	removable := make([]int, 0, len(summaries))
	for index := range summaries {
		reference := &summaries[index].Reference
		if reference.kind == ResumeStateLegacyUntrusted && reference.legacyRemovable {
			removable = append(removable, index)
		}
	}
	if len(removable) == 0 {
		return nil
	}
	duplicate, err := root.Duplicate()
	if err == nil {
		var same bool
		same, err = duplicate.SameDirectory(root)
		if !same {
			err = errors.Join(outputfault.ErrRootUnsafe, err)
		}
	}
	if err != nil {
		_ = closeOutputV3Directory(duplicate)
		for _, index := range removable {
			reference := &summaries[index].Reference
			reference.legacyRemovable = false
			summaries[index].Attention = append(summaries[index].Attention, ResumeAttention{
				Scope: ResumeAttentionLegacy, Code: "legacy-v2-root-pin-unavailable",
				State: reference.legacyName, Detail: err.Error(),
			})
		}
		return nil
	}
	rootPin := newResumeStateDirectoryPin(duplicate)
	defer func() { _ = rootPin.Close() }()
	for _, index := range removable {
		reference := &summaries[index].Reference
		markChanged := func(cause error) {
			reference.legacyRemovable = false
			summaries[index].Attention = append(summaries[index].Attention, ResumeAttention{
				Scope: ResumeAttentionLegacy, Code: "legacy-v2-journal-replaced",
				State: reference.legacyName, Detail: cause.Error(),
			})
		}
		kind, exact, err := root.ClassifyExactEntry(reference.legacyName)
		if err != nil || !exact || kind != outputcap.EntryRegularFile {
			markChanged(errors.Join(outputfault.ErrRootUnsafe, err))
			continue
		}
		entry, err := root.OpenEntry(reference.legacyName)
		if err != nil {
			markChanged(err)
			continue
		}
		if entry.Kind() != outputcap.EntryRegularFile {
			_ = entry.Close()
			markChanged(outputfault.ErrRootUnsafe)
			continue
		}
		matches, matchErr := root.EntryMatches(reference.legacyName, entry)
		file, openErr := root.OpenFile(reference.legacyName, false, false)
		var encoded []byte
		var readErr, closeErr error
		if openErr == nil {
			encoded, readErr = outputnamespace.ReadFile(file, maxLegacyOutputJournalBytes)
			closeErr = file.Close()
		}
		stable, stableErr := root.EntryMatches(reference.legacyName, entry)
		digest := sha256.Sum256(encoded)
		if matchErr != nil || !matches || openErr != nil || readErr != nil || closeErr != nil ||
			stableErr != nil || !stable || uint64(len(encoded)) != reference.legacySize ||
			digest != reference.legacyDigest {
			_ = entry.Close()
			markChanged(errors.Join(
				outputfault.ErrRootUnsafe, matchErr, openErr, readErr, closeErr, stableErr,
			))
			continue
		}
		if !rootPin.retain() {
			_ = entry.Close()
			markChanged(outputfault.ErrRootUnsafe)
			continue
		}
		reference.legacyRoot = rootPin
		reference.legacyPin = newResumeStateEntryPin(entry)
	}
	return nil
}

func unsafeIntentSummary(
	rootPath string,
	root resumestate.OutputRootBinding,
	intent transfer.ResumeIntent,
	intentName string,
	code string,
) ResumeStateSummary {
	return ResumeStateSummary{
		Reference: ResumeStateRef{
			rootPath: rootPath, root: root, intent: intent, kind: ResumeStateOpaqueUnsafe,
			namespaceName: intentName,
		},
		Attention: []ResumeAttention{{Scope: ResumeAttentionIntent, Code: code, State: intentName}},
	}
}

func unsafeSessionSummary(
	rootPath string,
	root resumestate.OutputRootBinding,
	intent transfer.ResumeIntent,
	intentName string,
	sessionName string,
	sessionKind outputcap.EntryKind,
	sessionPin *resumeStateEntryPin,
	code string,
) ResumeStateSummary {
	return ResumeStateSummary{
		Reference: ResumeStateRef{
			rootPath: rootPath, root: root, intent: intent, kind: ResumeStateOpaqueUnsafe,
			namespaceName: intentName, sessionName: sessionName,
			sessionKind: sessionKind, sessionPin: sessionPin,
		},
		Attention: []ResumeAttention{{Scope: ResumeAttentionIntent, Code: code, State: sessionName}},
	}
}

func discardLegacyState(root outputcap.Directory, reference ResumeStateRef) (DiscardSettlement, error) {
	if !reference.legacyRemovable || strings.HasPrefix(reference.legacyName, legacyOutputStagePrefix) {
		return DiscardSettlement{}, outputnamespace.RootFault("validate legacy discard state", outputfault.ErrLegacyState)
	}
	if root == nil {
		return DiscardSettlement{}, transfer.ErrInvalidOutputBinding
	}
	fixedRoot := reference.legacyRoot.fixedDirectory()
	if fixedRoot == nil {
		return DiscardSettlement{}, transfer.ErrInvalidOutputBinding
	}
	sameRoot, err := fixedRoot.SameDirectory(root)
	if err != nil || !sameRoot {
		return DiscardSettlement{}, outputnamespace.RootFault(
			"bind legacy discard root", errors.Join(outputfault.ErrRootUnsafe, err),
		)
	}
	entry := reference.legacyPin.take()
	if entry == nil {
		return DiscardSettlement{}, transfer.ErrInvalidOutputBinding
	}
	if entry.Kind() != outputcap.EntryRegularFile {
		_ = entry.Close()
		return DiscardSettlement{}, outputnamespace.RootFault("validate pinned legacy discard state", outputfault.ErrLegacyState)
	}
	matches, matchErr := root.EntryMatches(reference.legacyName, entry)
	if matchErr != nil || !matches {
		_ = entry.Close()
		return DiscardSettlement{}, outputnamespace.RootFault(
			"bind pinned legacy discard state", errors.Join(outputfault.ErrRootUnsafe, matchErr),
		)
	}
	file, openErr := root.OpenFile(reference.legacyName, false, false)
	var encoded []byte
	var readErr, fileCloseErr error
	if openErr == nil {
		encoded, readErr = outputnamespace.ReadFile(file, maxLegacyOutputJournalBytes)
		fileCloseErr = file.Close()
	}
	stable, stableErr := root.EntryMatches(reference.legacyName, entry)
	digest := sha256.Sum256(encoded)
	if openErr != nil || readErr != nil || fileCloseErr != nil || stableErr != nil || !stable ||
		uint64(len(encoded)) != reference.legacySize || digest != reference.legacyDigest {
		_ = entry.Close()
		return DiscardSettlement{}, outputnamespace.RootFault(
			"verify fixed legacy discard state",
			errors.Join(outputfault.ErrRootUnsafe, openErr, readErr, fileCloseErr, stableErr),
		)
	}
	allocated, allocationErr := entry.AllocatedSize()
	if allocationErr != nil {
		_ = entry.Close()
		return DiscardSettlement{}, outputnamespace.RootFault("preview legacy discard state", allocationErr)
	}
	removeErr := root.RemoveEntry(reference.legacyName, entry)
	syncErr := root.Sync()
	closeErr := entry.Close()
	if err := errors.Join(removeErr, syncErr, closeErr); err != nil {
		return DiscardSettlement{}, outputnamespace.RootFault("remove legacy discard state", err)
	}
	return DiscardSettlement{Kind: Discarded, RemovedBytes: allocated}, nil
}

type privateTreePreview struct {
	allocatedBytes uint64
	fileRecords    uint64
	entries        int
	attention      []ResumeAttention
}

func previewPrivateTree(root outputcap.Directory) (privateTreePreview, error) {
	preview := privateTreePreview{}
	if err := previewPrivateDirectory(root, "", 0, &preview); err != nil {
		return privateTreePreview{}, err
	}
	return preview, nil
}

func previewPrivateDirectory(
	directory outputcap.Directory,
	prefix string,
	depth int,
	preview *privateTreePreview,
) error {
	if depth > resumestate.MaxStateNestingDepth || preview.entries > resumeTreeEntryLimit {
		return outputfault.ErrInspectionLimit
	}
	names, err := directory.Names(resumeDirectoryChildLimit)
	if err != nil {
		return err
	}
	for _, name := range names {
		preview.entries++
		if preview.entries > resumeTreeEntryLimit {
			return outputfault.ErrInspectionLimit
		}
		state := name
		if prefix != "" {
			state = prefix + "/" + name
		}
		if err := previewPrivateEntry(directory, name, state, depth, preview); err != nil {
			return err
		}
	}
	return nil
}

func previewPrivateEntry(
	directory outputcap.Directory,
	name string,
	state string,
	depth int,
	preview *privateTreePreview,
) error {
	entry, err := directory.OpenEntry(name)
	if err != nil {
		return err
	}
	switch entry.Kind() {
	case outputcap.EntryRegularFile:
		return previewPrivateRegularFile(directory, name, state, entry, preview)
	case outputcap.EntryDirectory:
		return previewPrivateChildDirectory(directory, name, state, depth, entry, preview)
	case outputcap.EntryOther:
		if err := entry.Close(); err != nil {
			return err
		}
		preview.attention = append(preview.attention, ResumeAttention{
			Scope: ResumeAttentionFile, Code: "unsafe-private-entry", State: state,
		})
		return nil
	case outputcap.EntryAbsent:
		_ = entry.Close()
		return outputcap.ErrUnsafeNamespace
	default:
		_ = entry.Close()
		return outputcap.ErrUnsafeNamespace
	}
}

func previewPrivateRegularFile(
	directory outputcap.Directory,
	name string,
	state string,
	entry outputcap.CurrentEntryReference,
	preview *privateTreePreview,
) error {
	strict, strictErr := directory.OpenFile(name, true, false)
	if strictErr == nil {
		strictErr = strict.Close()
	}
	matches, matchErr := directory.EntryMatches(name, entry)
	if matchErr != nil || !matches {
		_ = entry.Close()
		return errors.Join(outputcap.ErrUnsafeNamespace, matchErr)
	}
	if strictErr != nil {
		preview.attention = append(preview.attention, ResumeAttention{
			Scope: ResumeAttentionFile, Code: "unsafe-private-entry", State: state,
			Detail: strictErr.Error(),
		})
	}
	allocated, sizeErr := entry.AllocatedSize()
	closeErr := entry.Close()
	if sizeErr != nil || closeErr != nil || math.MaxUint64-preview.allocatedBytes < allocated {
		return errors.Join(outputcap.ErrUnsafeNamespace, sizeErr, closeErr)
	}
	preview.allocatedBytes += allocated
	if strings.HasPrefix(state, resumestate.FilesDirectoryName+"/") && strings.HasSuffix(name, ".state") {
		preview.fileRecords++
	}
	return nil
}

func previewPrivateChildDirectory(
	directory outputcap.Directory,
	name string,
	state string,
	depth int,
	entry outputcap.CurrentEntryReference,
	preview *privateTreePreview,
) error {
	child, err := directory.OpenPinnedDirectory(entry, true)
	if err != nil {
		preview.attention = append(preview.attention, ResumeAttention{
			Scope: ResumeAttentionFile, Code: "unsafe-private-entry", State: state,
			Detail: err.Error(),
		})
		child, err = directory.OpenPinnedDirectory(entry, false)
	}
	if err != nil {
		_ = entry.Close()
		return err
	}
	err = previewPrivateDirectory(child, state, depth+1, preview)
	matches, matchErr := directory.EntryMatches(name, entry)
	closeErr := errors.Join(child.Close(), entry.Close())
	if matchErr != nil || !matches {
		err = errors.Join(err, matchErr, outputcap.ErrUnsafeNamespace)
	}
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	return nil
}
