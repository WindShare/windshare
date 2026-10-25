package lanes

import (
	diag "github.com/windshare/windshare/core/diagnostics"
)

const settlementEventName = "lane_settlement"

func (v SettlementSummary) DiagnosticRecord() diag.Record {
	return diag.New(settlementEventName, diag.Fields{"route": diag.Text(v.Route.DiagnosticName()),
		"delivered_blocks": diag.Uint(v.DeliveredBlocks), "delivered_bytes": diag.Uint(v.DeliveredBytes),
		"failed_block_attempts": diag.Uint(v.FailedBlockAttempts), "reassigned_blocks": diag.Uint(v.ReassignedBlocks), "incomplete": diag.Bool(v.Incomplete),
	}).Correlate(diag.Fields{"protocol_session_id": diag.Identity(v.ProtocolSessionID[:]),
		"lane_id": diag.Number(float64(v.Lane.ID)), "lane_epoch": diag.Number(float64(v.Lane.Epoch))})
}
