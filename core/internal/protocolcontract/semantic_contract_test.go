package protocolcontract

import (
	"fmt"
	"testing"
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

func semanticCases(t *testing.T) []any {
	t.Helper()
	limits := map[string]string{
		"minChunkBytes": fmt.Sprint(minChunkSize), "maxChunkBytes": fmt.Sprint(maxChunkSize),
		"maxFileBytes":        fmt.Sprint(maxFileBytes),
		"maxCatalogPageBytes": fmt.Sprint(maxCatalogPageBytes), "maxCatalogPageEntries": fmt.Sprint(maxCatalogPageEntries),
		"maxDirectoryEntries": fmt.Sprint(maxDirectoryEntries), "maxSelectedRoots": fmt.Sprint(maxSelectedRoots),
		"maxSelectedRootNameBytes": fmt.Sprint(maxSelectedRootNameBytes), "maxDescriptorBytes": fmt.Sprint(maxDescriptorBytes),
		"maxOpenBatch": fmt.Sprint(maxOpenBatch), "maxInitialRangesPerFile": fmt.Sprint(maxInitialRangesPerFile),
		"maxInitialRangesPerOpen": fmt.Sprint(maxInitialRangesPerOpen),
		"maxBlockRequestIndices":  fmt.Sprint(maxBlockRequestIndices), "leaseTTLSeconds": fmt.Sprint(leaseTTLSeconds),
		"leaseRenewWindowSeconds": fmt.Sprint(leaseRenewWindowSeconds), "leaseMaximumSeconds": fmt.Sprint(leaseMaximumSeconds),
		"revisionGraceSeconds": fmt.Sprint(revisionGraceSeconds), "maxFrameBytes": fmt.Sprint(maxFrameBytes),
		"logicalLanesSession":     fmt.Sprint(logicalLanesSession),
		"reassemblyOperation":     fmt.Sprint(maxBlockRecordBytes),
		"senderCrashGraceSeconds": fmt.Sprint(senderCrashGraceSeconds), "relayChallengeSeconds": fmt.Sprint(relayChallengeSeconds),
		"joinStartingSeconds": fmt.Sprint(joinStartingSeconds), "clientHelloReplaySeconds": fmt.Sprint(clientHelloReplaySeconds),
		"operationReplayWindow":        fmt.Sprint(operationReplayWindow),
		"relaySessionTombstoneSeconds": fmt.Sprint(relaySessionTombstoneSeconds),
		"maxOpaqueCiphertextBytes":     fmt.Sprint(maxOpaqueCiphertextBytes),
	}
	return []any{
		map[string]any{"name": "frozen-limits", "values": limits},
		map[string]any{
			"name":      "error-domains",
			"session":   map[string]uint16{"auth": 0x1001, "replay-sequence": 0x1002, "malformed": 0x1003, "version": 0x1004, "budget": 0x1005, "sender-signature": 0x1006, "illegal-terminal": 0x1007, "sender-stopped": sessionCodeSenderStopped},
			"directory": map[string]uint16{"stale": 0x2001, "permission": 0x2002, "collision": 0x2003, "too-wide": 0x2004, "budget": 0x2005, "permanent-io": 0x2006, "transient-io": 0x2007, "cancelled": 0x2008},
			"revision":  map[string]uint16{"stale": 0x3001, "not-found": 0x3002, "unreadable": 0x3003, "unsupported-stability": 0x3004, "quota": 0x3005, "lease-expired": 0x3006, "drift": 0x3007, "invalid-lease": 0x3008, "busy": 0x3009},
			"block":     map[string]uint16{"invalid-ref": 0x4001, "out-of-range": 0x4002, "object-auth": 0x4003, "fragment-conflict": 0x4004, "timeout": 0x4005, "cancelled": 0x4006},
			"peer":      map[string]uint16{"negotiation": 0x5001, "timeout": 0x5002, "candidates": 0x5003, "admission": 0x5004, "capacity": 0x500c},
		},
		map[string]any{"name": "relay-registration-errors", "codes": map[string]uint16{
			"malformed": 1, "unsupported-mode": 2, "share-id-collision": 3, "already-registered": 4,
			"challenge-expired": 5, "invalid-proof": 6, "descriptor-invalid": 7, "not-found": 8,
			"starting": 9, "admission": 10, "stopped": 11, "resume-stale": 12, "access-denied": 13, "access-unavailable": 14,
		}},
		map[string]any{
			"name": "relay-route-lifecycle", "crashGraceSeconds": fmt.Sprint(senderCrashGraceSeconds),
			"sessionTombstoneSeconds": fmt.Sprint(relaySessionTombstoneSeconds),
			"routeBudgetCounts":       []string{"starting", "live", "crash-grace", "stopped-tombstone"},
			"sessionBudgets": map[string][]string{
				"global":    {"provisional", "active", "ended-id-tombstone"},
				"per-share": {"provisional", "active"},
			},
			"sessionAdmissionSeconds": "30",
			"sessionAdmissionPhases":  []string{"awaiting_receiver", "awaiting_sender", "active"},
			"sessionCredit":           relaySessionCreditSemantics(),
			"stopStoreOutcomes":       []string{"committed", "definitely-not-committed", "unknown"},
			"explicitStop": []string{
				"per-route-storage-transaction", "durable-tombstone-before-ack", "exact-participant-cleanup-before-ack",
				"unknown-durability-fail-closed", "same-stop-id-resolution", "no-crash-grace", "permanent-reject-same-instance",
			},
			"unexpectedDisconnect":      []string{"drop-sessions", "enter-bounded-crash-grace", "immediate-retirement-during-stop-commit"},
			"stoppedTombstoneRetention": "until-future-authenticated-refresh",
		},
		map[string]any{"name": "operation-final-matrix", "operations": operationFinalMatrix()},
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
	}
}
