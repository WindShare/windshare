package protocolcontract

import (
	"fmt"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
)

func legalOperationFinals() map[string][]string {
	return map[string][]string{
		"renew-lease":    {"lease-result", "operation-error"},
		"release-lease":  {"operation-complete", "operation-error"},
		"request-blocks": {"operation-complete", "operation-error"},
	}
}

func operationFinalMatrix() []any {
	finals := legalOperationFinals()
	return []any{
		map[string]any{"request": "renew-lease", "legalFinals": finals["renew-lease"]},
		map[string]any{"request": "release-lease", "legalFinals": finals["release-lease"]},
		map[string]any{"request": "request-blocks", "legalFinals": finals["request-blocks"]},
	}
}

func zipMemberFailure(memberStarted bool) (string, string) {
	if memberStarted {
		return "pause-job", "paused"
	}
	return "skip-and-report", "completed-with-errors"
}

func zipMemberFailureCases() []any {
	notStartedAction, notStartedOutcome := zipMemberFailure(false)
	startedAction, startedOutcome := zipMemberFailure(true)
	return []any{
		map[string]any{"memberStarted": false, "action": notStartedAction, "jobOutcome": notStartedOutcome},
		map[string]any{"memberStarted": true, "action": startedAction, "jobOutcome": startedOutcome},
	}
}

func canonicalSelectionCase(t *testing.T, fixture *fixture) any {
	t.Helper()
	share, err := catalog.ShareInstanceFromBytes(fixture.shareInstance)
	if err != nil {
		t.Fatal(err)
	}
	root, err := catalog.DirectoryIDFromBytes(fixture.syntheticRoot)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := catalog.DirectoryIDFromBytes(fixture.directoryID)
	if err != nil {
		t.Fatal(err)
	}
	file, err := catalog.FileIDFromBytes(fixture.fileID)
	if err != nil {
		t.Fatal(err)
	}
	rootGeneration, err := catalog.DirectoryGenerationFromBytes(fixture.generation)
	if err != nil {
		t.Fatal(err)
	}
	directoryGenerationBytes := slices.Clone(fixture.generation)
	directoryGenerationBytes[len(directoryGenerationBytes)-1] ^= 0x3f
	directoryGeneration, err := catalog.DirectoryGenerationFromBytes(directoryGenerationBytes)
	if err != nil {
		t.Fatal(err)
	}
	modified, err := catalog.NewModifiedTime(
		1_700_000_200,
		123_000_000,
		catalog.TimePrecisionMilliseconds,
	)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := transfer.NewSelectionRules(false, []transfer.SelectionOverride{
		{DirectoryID: directory, Selected: true, Ancestors: []catalog.DirectoryID{root}},
		{FileID: file, Selected: true, Ancestors: []catalog.DirectoryID{root, directory}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := transfer.NewCanonicalSelectionRequest(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := transfer.NewOutputSelection(
		share,
		root,
		rootGeneration,
		[]transfer.OutputSelectionDirectory{{
			Path: "photos", DirectoryID: directory,
			Generation: directoryGeneration, ModifiedTime: modified,
		}},
		[]transfer.OutputSelectionFile{{
			Path: "photos/readme.txt", FileID: file,
			ParentDirectoryID: directory, ParentGeneration: directoryGeneration,
			ExpectedSize: 2_097_175, ModifiedTime: modified,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := transfer.NewCanonicalSelectionV1(request, plan)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"name":                   "canonical-selection-v1",
		"shareInstanceB64":       b64(fixture.shareInstance),
		"syntheticRootB64":       b64(fixture.syntheticRoot),
		"rootGenerationB64":      b64(fixture.generation),
		"directoryIdB64":         b64(fixture.directoryID),
		"directoryGenerationB64": b64(directoryGenerationBytes),
		"fileIdB64":              b64(fixture.fileID),
		"defaultSelected":        false,
		"modifiedSeconds":        "1700000200",
		"modifiedNanoseconds":    uint32(123_000_000),
		"modifiedPrecision":      uint8(catalog.TimePrecisionMilliseconds),
		"expectedSize":           "2097175",
		"canonicalRequestB64":    b64(request.Bytes()),
		"canonicalSelectionB64":  b64(canonical.Bytes()),
		"selectionIdentityB64":   b64(plan.Identity().Bytes()),
		"resumeIntentB64":        b64(canonical.ResumeIntent().Bytes()),
	}
}

func semanticCases(t *testing.T, fixture *fixture) []any {
	t.Helper()
	limits := map[string]string{
		"minChunkBytes": fmt.Sprint(minChunkSize), "maxChunkBytes": fmt.Sprint(maxChunkSize),
		"segmentBytes": fmt.Sprint(segmentBytes), "maxFileBytes": fmt.Sprint(maxFileBytes),
		"maxCatalogPageBytes": fmt.Sprint(maxCatalogPageBytes), "maxCatalogPageEntries": fmt.Sprint(maxCatalogPageEntries),
		"maxDirectoryEntries": fmt.Sprint(maxDirectoryEntries), "maxSelectedRoots": fmt.Sprint(maxSelectedRoots),
		"maxSelectedRootNameBytes": fmt.Sprint(maxSelectedRootNameBytes), "maxDescriptorBytes": fmt.Sprint(maxDescriptorBytes),
		"maxOpenBatch": fmt.Sprint(maxOpenBatch), "maxInitialRangesPerFile": fmt.Sprint(maxInitialRangesPerFile),
		"maxInitialRangesPerOpen": fmt.Sprint(maxInitialRangesPerOpen),
		"maxBlockRequestIndices":  fmt.Sprint(maxBlockRequestIndices), "leaseTTLSeconds": fmt.Sprint(leaseTTLSeconds),
		"leaseRenewWindowSeconds": fmt.Sprint(leaseRenewWindowSeconds), "leaseMaximumSeconds": fmt.Sprint(leaseMaximumSeconds),
		"revisionGraceSeconds": fmt.Sprint(revisionGraceSeconds), "maxFrameBytes": fmt.Sprint(maxFrameBytes),
		"scanConcurrencySession": fmt.Sprint(scanConcurrencySession), "scanConcurrencyShare": fmt.Sprint(scanConcurrencyShare),
		"scanConcurrencyProcess": fmt.Sprint(scanConcurrencyProcess), "scanWorkSession": fmt.Sprint(scanWorkSession),
		"scanWorkShare": fmt.Sprint(scanWorkShare), "scanWorkProcess": fmt.Sprint(scanWorkProcess),
		"committedEntriesShare": fmt.Sprint(committedEntriesShare), "committedEntriesProcess": fmt.Sprint(committedEntriesProcess),
		"catalogMemorySession": fmt.Sprint(catalogMemorySession), "catalogMemoryShare": fmt.Sprint(catalogMemoryShare),
		"catalogMemoryProcess": fmt.Sprint(catalogMemoryProcess), "catalogSpillShare": fmt.Sprint(catalogSpillShare),
		"catalogSpillProcess": fmt.Sprint(catalogSpillProcess), "activeLeasesSession": fmt.Sprint(activeLeasesSession),
		"activeLeasesShare": fmt.Sprint(activeLeasesShare), "activeLeasesProcess": fmt.Sprint(activeLeasesProcess),
		"stableHandlesSession": fmt.Sprint(activeLeasesSession), "stableHandlesShare": fmt.Sprint(activeLeasesShare),
		"stableHandlesProcess": fmt.Sprint(activeLeasesProcess), "activeLanesSession": fmt.Sprint(activeLanesSession),
		"logicalLanesSession": fmt.Sprint(activeLanesSession),
		"activeLanesShare":    fmt.Sprint(activeLanesShare), "activeLanesProcess": fmt.Sprint(activeLanesProcess),
		"sealedCacheShare": fmt.Sprint(sealedCacheShare), "sealedCacheProcess": fmt.Sprint(sealedCacheProcess),
		"receiverCacheSession": fmt.Sprint(receiverCacheSession), "receiverCacheProcess": fmt.Sprint(receiverCacheProcess),
		"reassemblyOperation": fmt.Sprint(maxBlockRecordBytes), "reassemblySession": fmt.Sprint(reassemblySession),
		"reassemblyShare": fmt.Sprint(reassemblyShare), "reassemblyProcess": fmt.Sprint(reassemblyProcess),
		"reassemblyRecordsSession": fmt.Sprint(reassemblyRecordsSession), "reassemblyRecordsShare": fmt.Sprint(reassemblyRecordsShare),
		"reassemblyRecordsProcess": fmt.Sprint(reassemblyRecordsProcess), "controlQueueFrames": fmt.Sprint(controlQueueFrames),
		"controlQueueBytes": fmt.Sprint(controlQueueBytes), "dataQueueFrames": fmt.Sprint(dataQueueFrames),
		"dataQueueBytes": fmt.Sprint(dataQueueBytes), "maxDataFairnessBurst": fmt.Sprint(maxDataFairnessBurst),
		"senderCrashGraceSeconds": fmt.Sprint(senderCrashGraceSeconds), "relayChallengeSeconds": fmt.Sprint(relayChallengeSeconds),
		"joinStartingSeconds": fmt.Sprint(joinStartingSeconds), "clientHelloReplaySeconds": fmt.Sprint(clientHelloReplaySeconds),
		"operationTombstoneSeconds": fmt.Sprint(operationTombstoneSeconds), "applicationRelaySeconds": fmt.Sprint(applicationRelaySeconds),
		"relaySessionTombstoneSeconds": fmt.Sprint(relaySessionTombstoneSeconds),
		"maxOpaqueCiphertextBytes":     fmt.Sprint(maxOpaqueCiphertextBytes),
		"opfsStagingJobBytes":          fmt.Sprint(opfsStagingJobBytes), "opfsStagingProcessBytes": fmt.Sprint(opfsStagingProcessBytes),
		"opfsMinimumReserveBytes": fmt.Sprint(opfsMinimumReserveBytes), "outputOpenTransactions": fmt.Sprint(outputOpenTransactions),
	}
	selection := []any{
		map[string]any{"files": "29", "bytes": fmt.Sprint((8 << 20) - 1), "terminal": true, "failed": false, "class": "small"},
		map[string]any{"files": "30", "bytes": "0", "terminal": true, "failed": false, "class": "large"},
		map[string]any{"files": "1", "bytes": fmt.Sprint(8 << 20), "terminal": true, "failed": false, "class": "large"},
		map[string]any{"files": "30", "bytes": "0", "terminal": false, "failed": false, "class": "large"},
		map[string]any{"files": "1", "bytes": fmt.Sprint(8 << 20), "terminal": true, "failed": true, "class": "large"},
		map[string]any{"files": "0", "bytes": "0", "terminal": false, "failed": false, "class": "unknown"},
		map[string]any{"files": "0", "bytes": "0", "terminal": true, "failed": true, "class": "unknown"},
		map[string]any{"files": "0", "bytes": "0", "terminal": true, "failed": false, "class": "small"},
	}
	checkpointCuts := []any{
		map[string]any{"cut": "after-data-write", "published": false},
		map[string]any{"cut": "after-data-flush", "published": false},
		map[string]any{"cut": "after-journal-write", "published": false},
		map[string]any{"cut": "after-journal-flush", "published": false},
		map[string]any{"cut": "after-install", "published": false},
		map[string]any{"cut": "after-reopen-verify", "published": true},
	}
	return []any{
		map[string]any{"name": "frozen-limits", "values": limits},
		map[string]any{
			"name":      "error-domains",
			"session":   map[string]uint16{"auth": 0x1001, "replay-sequence": 0x1002, "malformed": 0x1003, "version": 0x1004, "budget": 0x1005, "sender-signature": 0x1006, "illegal-terminal": 0x1007, "sender-stopped": sessionCodeSenderStopped},
			"directory": map[string]uint16{"stale": 0x2001, "permission": 0x2002, "collision": 0x2003, "too-wide": 0x2004, "budget": 0x2005, "permanent-io": 0x2006, "transient-io": 0x2007, "cancelled": 0x2008},
			"revision":  map[string]uint16{"stale": 0x3001, "not-found": 0x3002, "unreadable": 0x3003, "unsupported-stability": 0x3004, "quota": 0x3005, "lease-expired": 0x3006, "drift": 0x3007, "invalid-lease": 0x3008},
			"block":     map[string]uint16{"invalid-ref": 0x4001, "out-of-range": 0x4002, "object-auth": 0x4003, "fragment-conflict": 0x4004, "timeout": 0x4005, "cancelled": 0x4006},
			"peer":      map[string]uint16{"negotiation": 0x5001, "timeout": 0x5002, "candidates": 0x5003, "admission": 0x5004},
		},
		map[string]any{"name": "relay-registration-errors", "codes": map[string]uint16{
			"malformed": 1, "unsupported-mode": 2, "share-id-collision": 3, "already-registered": 4,
			"challenge-expired": 5, "invalid-proof": 6, "descriptor-invalid": 7, "not-found": 8,
			"starting": 9, "admission": 10, "stopped": 11,
		}},
		map[string]any{
			"name": "relay-route-lifecycle", "crashGraceSeconds": fmt.Sprint(senderCrashGraceSeconds),
			"sessionTombstoneSeconds": fmt.Sprint(relaySessionTombstoneSeconds),
			"routeBudgetCounts":       []string{"starting", "live", "crash-grace", "stopped-tombstone"},
			"sessionBudgetCounts":     []string{"active", "ended-id-tombstone"},
			"sessionBudgetScopes":     []string{"global", "per-share"},
			"stopStoreOutcomes":       []string{"committed", "definitely-not-committed", "unknown"},
			"explicitStop": []string{
				"per-route-storage-transaction", "durable-tombstone-before-ack", "exact-participant-cleanup-before-ack",
				"unknown-durability-fail-closed", "same-stop-id-resolution", "no-crash-grace", "permanent-reject-same-instance",
			},
			"unexpectedDisconnect":      []string{"drop-sessions", "enter-bounded-crash-grace", "immediate-retirement-during-stop-commit"},
			"stoppedTombstoneRetention": "until-future-authenticated-refresh",
		},
		map[string]any{"name": "selection-classification", "cases": selection, "fileLimitExclusive": "30", "byteLimitExclusive": fmt.Sprint(8 << 20)},
		canonicalSelectionCase(t, fixture),
		map[string]any{"name": "operation-final-matrix", "operations": operationFinalMatrix()},
		map[string]any{"name": "connection-timing", "triggers": []any{
			map[string]any{"trigger": "browse", "startsP2P": false, "p2pStartSeconds": nil, "applicationRelayDeadlineSeconds": nil, "outputPicker": "none"},
			map[string]any{"trigger": "preview-click", "startsP2P": true, "p2pStartSeconds": "0", "applicationRelayDeadlineSeconds": fmt.Sprint(applicationRelaySeconds), "outputPicker": "none"},
			map[string]any{"trigger": "download-click", "startsP2P": true, "p2pStartSeconds": "0", "applicationRelayDeadlineSeconds": fmt.Sprint(applicationRelaySeconds), "outputPicker": "synchronous"},
		}, "independentTimers": true, "discoveryCannotDelay": true, "unknownUsesNonSmallTiming": true, "turnInsertionOnly": true},
		map[string]any{"name": "strict-sequence", "cases": []any{
			map[string]any{"epoch": uint32(0), "expected": "0", "candidate": "0", "accepted": true},
			map[string]any{"epoch": uint32(0), "expected": "1", "candidate": "0", "accepted": false},
			map[string]any{"epoch": uint32(0), "expected": "1", "candidate": "2", "accepted": false},
			map[string]any{"epoch": uint32(1), "expected": "0", "candidate": "0", "accepted": true},
			map[string]any{"epoch": uint32(0), "expected": "closed", "candidate": "1", "accepted": false},
		}},
		map[string]any{"name": "lane-epoch-acceptance", "globallyAllocated": []uint32{1, 2, 3, 4, 5, 6, 7}, "cases": []any{
			map[string]any{"lane": uint32(1), "lastAccepted": uint32(3), "candidate": uint32(5), "accepted": true},
			map[string]any{"lane": uint32(1), "lastAccepted": uint32(5), "candidate": uint32(5), "accepted": false},
			map[string]any{"lane": uint32(1), "lastAccepted": uint32(5), "candidate": uint32(4), "accepted": false},
			map[string]any{"lane": uint32(2), "lastAccepted": nil, "candidate": uint32(4), "otherLaneLast": uint32(7), "accepted": true},
		}},
		map[string]any{"name": "output-checkpoint-crash-cuts", "order": []string{"data-write", "data-flush", "journal-write", "journal-flush", "atomic-install", "reopen-verify"}, "cuts": checkpointCuts},
		map[string]any{"name": "output-backend-capabilities", "backends": []any{
			map[string]any{"backend": "fsa", "durability": "none-until-reauthorization-and-reopen-proof", "randomWrite": true, "fileFailureIsolation": true, "mtime": false, "powerLoss": false},
			map[string]any{"backend": "opfs-staging", "durability": "process-restart", "randomWrite": true, "fileFailureIsolation": true, "mtime": false, "powerLoss": false},
			map[string]any{"backend": "single-file-stream", "durability": "none", "randomWrite": false, "fileFailureIsolation": false, "mtime": false, "failureAfterFirstByte": "pause-job"},
			map[string]any{"backend": "zip-stream", "durability": "none", "randomWrite": false, "fileFailureIsolation": false, "mtime": false, "memberStart": "first-local-file-header-byte"},
			map[string]any{"backend": "cli-osfs", "durability": "process-restart", "randomWrite": true, "fileFailureIsolation": true, "mtime": true, "powerLoss": false},
		}},
		map[string]any{"name": "zip-member-failure", "cases": zipMemberFailureCases()},
		map[string]any{"name": "catalog-transaction", "publishOnlyAfter": []string{"pages", "node-records", "terminal", "budget-charge", "spill-flush", "atomic-commit"}, "preCommitCrashVisible": false},
		map[string]any{"name": "stable-source-platforms", "platforms": []any{
			map[string]any{"platform": "windows-local-ntfs-refs", "mechanism": "deny-share-write-handle+volume-file-id", "supported": true},
			map[string]any{"platform": "linux-local-regular", "mechanism": "device+inode+size+mtime-ns+ctime-ns", "supported": true},
			map[string]any{"platform": "darwin-local-regular", "mechanism": "device+inode+size+mtime-ns+ctime-ns", "supported": true},
			map[string]any{"platform": "other-network-pseudo", "mechanism": "unsupported-stability", "supported": false},
		}},
		map[string]any{
			"name": "offline-lifecycle", "states": []string{"preparing", "live-only", "offline-uploading", "offline-committed", "stopping", "stopped"},
			"transitions": []any{
				map[string]any{"from": "preparing", "event": "registered", "to": "live-only"},
				map[string]any{"from": "preparing", "event": "stop", "to": "stopping"},
				map[string]any{"from": "live-only", "event": "begin-offline", "to": "offline-uploading"},
				map[string]any{"from": "live-only", "event": "stop", "to": "stopping"},
				map[string]any{"from": "offline-uploading", "event": "commit-ack", "to": "offline-committed"},
				map[string]any{"from": "offline-uploading", "event": "stop", "to": "stopping"},
				map[string]any{"from": "stopping", "event": "cleanup-complete", "to": "stopped"},
				map[string]any{"from": "offline-committed", "event": "sender-exit", "to": "offline-committed"},
			},
			"explicitStopEffects":        []string{"reject-join", "signed-session-terminal", "cancel-scan-revision-lanes", "cancel-uncommitted-upload", "challenged-signed-stop", "cleanup-staging"},
			"explicitStopUsesCrashGrace": false, "crashGraceSeconds": "60",
			"unexpectedDisconnectStates": []string{"live-only", "offline-uploading"},
		},
	}
}
