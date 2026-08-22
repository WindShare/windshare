package protocolcontract

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"testing"
)

const (
	directZipAutomaticMaxPrefixCopyBytes           uint64 = 256 << 20
	directZipAutomaticMaxCumulativeCopyBytes       uint64 = 512 << 20
	directZipAutomaticMaxModeledPeakTemporaryBytes uint64 = 256 << 20
	zipWorkspaceRecommendationMaximumPeakBytes     uint64 = 1_073_744_986
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

func zipCompleteOnlyFailure() (string, string) {
	return "abort-artifact", "failed"
}

func zipCompleteOnlyFailureCases() []any {
	action, outcome := zipCompleteOnlyFailure()
	return []any{
		map[string]any{"failure": "discovery", "action": action, "artifactOutcome": outcome, "publicationAllowed": false, "partialResult": false},
		map[string]any{"failure": "member-before-header", "action": action, "artifactOutcome": outcome, "publicationAllowed": false, "partialResult": false},
		map[string]any{"failure": "member-after-header", "action": action, "artifactOutcome": outcome, "publicationAllowed": false, "partialResult": false},
	}
}

func directZipEpochPolicyDigestV1() string {
	preimage := append([]byte("windshare/direct-zip-epoch-policy/v1\x00"), 1)
	for _, value := range []uint64{
		directZipAutomaticMaxPrefixCopyBytes,
		directZipAutomaticMaxCumulativeCopyBytes,
		directZipAutomaticMaxModeledPeakTemporaryBytes,
	} {
		var frame [16]byte
		binary.BigEndian.PutUint64(frame[:8], 8)
		binary.BigEndian.PutUint64(frame[8:], value)
		preimage = append(preimage, frame[:]...)
	}
	digest := sha256.Sum256(preimage)
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func TestDirectZipEpochPolicyV1(t *testing.T) {
	if got, want := directZipEpochPolicyDigestV1(), "dVc_DFPK_50xrZ7_GK0oQ9noWgHhb-2eZEnl4-0kUOo"; got != want {
		t.Fatalf("DirectZipEpochPolicyV1 digest = %q, want %q", got, want)
	}
	// The measured 256 MiB predecessor acquired a 1 MiB ZIP epoch before restart.
	if measuredCommittedArchivePrefix := uint64(257 << 20); measuredCommittedArchivePrefix <= directZipAutomaticMaxPrefixCopyBytes {
		t.Fatal("257 MiB committed ZIP prefix passed the inclusive 256 MiB automatic boundary")
	}
}

func zipRouteRecommendationPolicyDigestV1() string {
	preimage := append([]byte("windshare/zip-route-recommendation-policy/v1\x00"), 1)
	var frame [16]byte
	binary.BigEndian.PutUint64(frame[:8], 8)
	binary.BigEndian.PutUint64(frame[8:], zipWorkspaceRecommendationMaximumPeakBytes)
	digest := sha256.Sum256(append(preimage, frame[:]...))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func TestZipRouteRecommendationPolicyV1(t *testing.T) {
	if got, want := zipRouteRecommendationPolicyDigestV1(), "zHRGRc5-OvZ4Z8U2E1ORwNWnccnf_p35QB8iSXlixqI"; got != want {
		t.Fatalf("ZipRouteRecommendationPolicyV1 digest = %q, want %q", got, want)
	}
	if got, want := zipWorkspaceRecommendationMaximumPeakBytes*2, uint64(2_147_489_972); got != want {
		t.Fatalf("1 GiB raw modeled peak = %d, want %d", got, want)
	}
}

func semanticCases(t *testing.T) []any {
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
		"relaySessionTombstoneSeconds":        fmt.Sprint(relaySessionTombstoneSeconds),
		"maxOpaqueCiphertextBytes":            fmt.Sprint(maxOpaqueCiphertextBytes),
		"defaultOpfsJobWorkspaceLimit":        fmt.Sprint(defaultOPFSJobWorkspaceLimit),
		"defaultOpfsProcessWorkspaceLimit":    fmt.Sprint(defaultOPFSProcessWorkspaceLimit),
		"minimumOpfsQuotaReserve":             fmt.Sprint(minimumOPFSQuotaReserve),
		"defaultPortableHandoffArtifactLimit": fmt.Sprint(defaultPortableArtifactLimit),
	}
	connectionSizes := []any{
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
		map[string]any{"cut": "after-data-write", "newGenerationSelectable": false},
		map[string]any{"cut": "after-data-flush", "newGenerationSelectable": false},
		map[string]any{"cut": "after-candidate-record-write", "newGenerationSelectable": false},
		map[string]any{"cut": "after-candidate-record-flush", "newGenerationSelectable": false},
		map[string]any{"cut": "after-verified-record-install", "newGenerationSelectable": false},
		map[string]any{"cut": "after-reopen-verify", "newGenerationSelectable": true},
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
		map[string]any{"name": "connection-size-classification", "cases": connectionSizes, "fileLimitExclusive": "30", "byteLimitExclusive": fmt.Sprint(8 << 20)},
		map[string]any{
			"name": "artifact-shape-proof",
			"proofs": []any{
				map[string]any{"proof": "unknown", "byte": 1},
				map[string]any{"proof": "none", "byte": 2},
				map[string]any{"proof": "single-file", "byte": 3},
				map[string]any{"proof": "tree", "byte": 4},
			},
			"allowedTransitions": []string{"unknown->none", "unknown->single-file", "unknown->tree"},
			"treeForcingFacts":   []string{"authenticated-selected-directory", "explicit-empty-directory", "second-authenticated-file"},
			"singleFileRequires": []string{"one-authenticated-file", "frozen-rule-exclusion", "empty-unsettled-targets"},
			"noneRequires":       []string{"complete-negative-evidence"}, "finalProofImmutable": true,
		},
		map[string]any{"name": "operation-final-matrix", "operations": operationFinalMatrix()},
		map[string]any{"name": "artifact-action-picker-timing", "events": []any{
			map[string]any{"event": "background-projection", "startsP2P": false, "picker": "forbidden"},
			map[string]any{"event": "preview-click", "startsP2P": true, "picker": "none"},
			map[string]any{"event": "final-artifact-action-without-picker", "startsP2P": true, "picker": "none"},
			map[string]any{"event": "final-artifact-action-with-picker", "startsP2P": true, "picker": "synchronous-before-click-stack-unwinds"},
		}, "p2pStartSeconds": "0", "applicationRelayDeadlineSeconds": fmt.Sprint(applicationRelaySeconds),
			"backgroundCompletionCannotInvokeAction": true, "authorityValidationMayContinueAfterPickerStart": true,
			"bindRechecks": []string{"projection-epoch", "shape-proof", "artifact-offer", "capability-facts"}},
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
		map[string]any{
			"name": "file-checkpoint-v2-crash-cuts", "ownershipMarker": "windshare/file-checkpoint/v2",
			"namespace": ".windshare-output/checkpoints-v2", "selectionAuthority": "highest-reopened-verified-generation",
			"order": []string{"data-write", "data-flush", "candidate-record-write", "candidate-record-flush", "verified-record-install", "reopen-verify"},
			"cuts":  checkpointCuts,
		},
		map[string]any{"name": "artifact-plan-guarantee-matrix", "rows": []any{
			map[string]any{"artifact": "directory-tree", "layout": "single-file-or-result-root", "plan": "direct-tree", "binding": "named-container-entry", "guaranteeProfiles": []string{"native-tree", "fsa-tree"}, "preparation": "none", "completion": "prefix-visible-partial-legal"},
			map[string]any{"artifact": "directory-tree", "layout": "catalog-root", "plan": "direct-tree", "binding": "container-root", "guaranteeProfiles": []string{"native-tree"}, "preparation": "none", "completion": "prefix-visible"},
			map[string]any{"artifact": "original-file", "layout": "single-file-proof", "plan": "direct-atomic", "binding": "atomic-target", "guaranteeProfiles": []string{"managed-atomic"}, "preparation": "none", "completion": "published-after-verified-commit"},
			map[string]any{
				"artifact": "zip-archive", "layout": "result-root", "plan": "direct-resumable-zip",
				"binding": "fsa-owned-file", "guaranteeProfiles": []string{"fsa-owned-file"},
				"preparation": "none", "completion": "verified-complete-only",
				"targetVisibility":     "operation-owned-incomplete-file-visible",
				"artifactAvailability": "verified-complete-only",
				"cleanupAuthority":     "ownership-proof-required",
			},
			map[string]any{"artifact": "original-file", "layout": "single-file-proof", "plan": "workspace-then-publish", "binding": "origin-private-workspace", "guaranteeProfiles": []string{"managed-atomic", "browser-handoff"}, "preparation": "none", "completion": "sealed-then-waiting-to-save"},
			map[string]any{"artifact": "zip-archive", "layout": "result-root", "plan": "workspace-then-publish", "binding": "origin-private-workspace", "guaranteeProfiles": []string{"managed-atomic", "browser-handoff"}, "preparation": "exact-zip", "completion": "complete-only-sealed-then-waiting-to-save"},
			map[string]any{"artifact": "original-file-or-zip-archive", "layout": "explicit-artifact", "plan": "portable-handoff", "binding": "portable", "guaranteeProfiles": []string{"browser-handoff"}, "preparation": "exact-artifact", "completion": "download-started-only"},
		}},
		map[string]any{
			"name":        "workspace-budget-v1",
			"components":  []string{"uniqueRawBytes", "packageBytes", "peakTemporaryBytes", "durableMetadataBytes"},
			"derivedPeak": "checked-sum-components", "ownedObjectCountedOnce": true, "quotaEstimateIsReservation": false,
			"limits": map[string]string{
				"DEFAULT_OPFS_JOB_WORKSPACE_LIMIT":        fmt.Sprint(defaultOPFSJobWorkspaceLimit),
				"DEFAULT_OPFS_PROCESS_WORKSPACE_LIMIT":    fmt.Sprint(defaultOPFSProcessWorkspaceLimit),
				"MINIMUM_OPFS_QUOTA_RESERVE":              fmt.Sprint(minimumOPFSQuotaReserve),
				"DEFAULT_PORTABLE_HANDOFF_ARTIFACT_LIMIT": fmt.Sprint(defaultPortableArtifactLimit),
			},
			"admissionChecks": []string{"job-peak", "process-active-job-peaks", "quota-minus-usage-minus-reserve", "every-allocation"},
		},
		map[string]any{"name": "zip-complete-only", "encoding": "store", "completeness": "complete-only", "cases": zipCompleteOnlyFailureCases()},
		map[string]any{
			"name": "receive-lifecycle-v2", "domain": "windshare/receive-lifecycle-state/v2", "schemaVersion": 2,
			"terminalStates": []any{
				map[string]any{"state": "published", "byte": 14, "plans": []string{"direct-tree", "direct-atomic", "workspace-then-publish", "direct-resumable-zip"}},
				map[string]any{"state": "download-started", "byte": 15, "plans": []string{"workspace-then-publish", "portable-handoff"}},
				map[string]any{"state": "partial-directory", "byte": 16, "plans": []string{"direct-tree"}},
				map[string]any{"state": "restart-required", "byte": 17, "plans": []string{"direct-atomic", "portable-handoff", "direct-resumable-zip"}},
				map[string]any{"state": "discarded", "byte": 18, "plans": []string{"direct-tree", "direct-atomic", "workspace-then-publish", "portable-handoff", "direct-resumable-zip"}},
				map[string]any{"state": "expired", "byte": 19, "plans": []string{"direct-tree", "workspace-then-publish", "direct-resumable-zip"}},
				map[string]any{"state": "needs-attention", "byte": 20, "plans": []string{"direct-tree", "direct-atomic", "workspace-then-publish", "direct-resumable-zip"}},
			},
			"nonterminalRecoveryStates": []any{
				map[string]any{"state": "authorization-required", "byte": 21},
				map[string]any{"state": "target-verification-required", "byte": 22},
				map[string]any{"state": "destination-space-required", "byte": 23},
			},
			"restartReasons": map[string]int{
				"direct-atomic-rolled-back": 1, "portable-aborted": 2, "source-revision-changed": 3,
				"preparation-invalidated": 4, "content-session-ended": 5, "target-deleted": 6,
			},
			"resumableReceivePayloadKinds": map[string]int{"file-set": 1, "direct-zip": 2},
			"directZipByteSemantics": map[string]string{
				"receivedBytes":          "selected-source-payload-bytes-received-in-live-attempt",
				"safeResumeBytes":        "selected-source-payload-bytes-covered-by-verified-checkpoint",
				"committedArchiveLength": "verified-target-prefix-bytes",
			},
			"deadlineWritingStates": []string{
				"resumable-receive", "resumable-package", "waiting-to-save",
				"authorization-required", "target-verification-required", "destination-space-required",
			},
			"publishedCleanupPendingRemains": "published", "handoffNeverMeans": "published",
			"completeArtifactsExclude": []string{"partial-directory"},
		},
		map[string]any{
			"name": "direct-zip-contract-policy-v1", "routeSupport": "available-exact-reviewed-platform-only",
			"ownershipExtraFormat": map[string]any{
				"domain": "windshare/direct-zip-ownership-extra/v1", "availability": "frozen",
				"digest": "hnFdK_xeDeYInsyhv5i4YdU57UUcWeAGUYLTakWaZQw",
			},
			"choiceIdentity": map[string]any{
				"domain": "windshare/artifact-choice/v1", "materializationByte": 5,
				"directZipArtifactChoiceId":    "0dkx9vDTzvH7B7a9EUoJBOWLCWgmVwLoFH3jjRmfHFU",
				"workspaceZipArtifactChoiceId": "RW0aXukzHVFiMjNEaoYb8qGKTN-AKAhw7u-Yi_-WsoQ",
			},
			"policies": []any{
				map[string]any{"name": "zip-encoding-v2", "domain": "windshare/zip-encoding/v2-store-data-descriptor-owned-marker", "availability": "frozen", "digest": "LWNj2jiL6U3tTZNaLy5txjFlDSaoUzhrjT0J44r0drc"},
				map[string]any{"name": "direct-zip-layout-v2", "domain": "windshare/zip-layout/v2-paged-owned-marker", "availability": "frozen", "digest": "VSV-D1TwhzhxuCZYgx1-ZEE26oFu-mHXA4oWELOgGH4"},
				map[string]any{"name": "direct-zip-checkpoint-v1", "domain": "windshare/direct-zip-checkpoint-policy/v1", "availability": "frozen"},
				map[string]any{"name": "direct-zip-journal-budget-v1", "domain": "windshare/direct-zip-journal-budget/v1", "availability": "frozen"},
				map[string]any{
					"name": "direct-zip-epoch-v1", "domain": "windshare/direct-zip-epoch-policy/v1", "availability": "frozen",
					"automaticMaxPrefixCopyBytes":           fmt.Sprint(directZipAutomaticMaxPrefixCopyBytes),
					"automaticMaxCumulativeCopyBytes":       fmt.Sprint(directZipAutomaticMaxCumulativeCopyBytes),
					"automaticMaxModeledPeakTemporaryBytes": fmt.Sprint(directZipAutomaticMaxModeledPeakTemporaryBytes),
					"units":                                 "committed-archive-bytes", "boundary": "inclusive", "digest": directZipEpochPolicyDigestV1(),
				},
				map[string]any{
					"name": "zip-route-recommendation-v1", "domain": "windshare/zip-route-recommendation-policy/v1", "availability": "frozen",
					"boundary": "inclusive", "exactWorkspaceRecommendationBudget": fmt.Sprint(zipWorkspaceRecommendationMaximumPeakBytes),
					"digest": zipRouteRecommendationPolicyDigestV1(), "semantics": "display-ranking-only",
				},
			},
			"processRestart": "reviewed-exact-platform-only",
		},
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
