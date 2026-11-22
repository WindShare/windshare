import type {
  AdminConfigPatch,
  CommitDonationOutcome,
  DonationProvider,
  NonPrivateVisibilityKind,
} from './model'

export type WebhookRejectionReason =
  | 'invalid_signature'
  | 'afdian_verify_failed'
  | 'invalid_payload'
  | 'body_too_large'

export type AfdianOrderSkippedEvent =
  | { readonly outTradeNo: string; readonly reason: 'order_not_found' }
  | { readonly outTradeNo: string; readonly reason: 'order_not_paid'; readonly orderStatus: number }

export interface NowPaymentsIntentFallbackEvent {
  readonly paymentId: string
  readonly orderId: string
  readonly reason: 'not_found_or_expired'
}

function emit(level: 'info' | 'warn' | 'error', event: string, fields: Record<string, unknown>): void {
  const line = JSON.stringify({ event, ...fields })
  if (level === 'error') {
    console.error(line)
  } else if (level === 'warn') {
    console.warn(line)
  } else {
    console.log(line)
  }
}

export function logCryptoIntentCreated(fields: {
  readonly intentId: string
  readonly requestedUsdMinor: number
  readonly hasMessage: boolean
}): void {
  emit('info', 'donation.crypto_intent_created', {
    intent_id: fields.intentId,
    requested_usd_minor: fields.requestedUsdMinor,
    has_message: fields.hasMessage,
  })
}

export function logCryptoInvoiceFailed(fields: {
  readonly intentId: string
  readonly reason: string
}): void {
  emit('warn', 'donation.crypto_invoice_failed', {
    intent_id: fields.intentId,
    reason: fields.reason,
  })
}

export function logWebhookReceived(fields: {
  readonly provider: DonationProvider
  readonly externalId: string
  readonly status: string
}): void {
  emit('info', 'donation.webhook_received', {
    provider: fields.provider,
    external_id: fields.externalId,
    status: fields.status,
  })
}

export function logWebhookRejected(fields: {
  readonly provider: DonationProvider
  readonly reason: WebhookRejectionReason
  readonly externalId?: string
  readonly upstreamReason?: string
}): void {
  const payload: Record<string, unknown> = {
    provider: fields.provider,
    reason: fields.reason,
  }
  if (fields.externalId !== undefined) {
    payload.external_id = fields.externalId
  }
  if (fields.upstreamReason !== undefined) {
    payload.upstream_reason = fields.upstreamReason
  }
  emit('warn', 'donation.webhook_rejected', payload)
}

export function logAfdianOrderSkipped(fields: AfdianOrderSkippedEvent): void {
  if (fields.reason === 'order_not_found') {
    emit('warn', 'donation.afdian_order_skipped', {
      out_trade_no: fields.outTradeNo,
      reason: fields.reason,
    })
    return
  }
  emit('info', 'donation.afdian_order_skipped', {
    out_trade_no: fields.outTradeNo,
    reason: fields.reason,
    order_status: fields.orderStatus,
  })
}

export function logAfdianSponsorLookupFallback(fields: {
  readonly outTradeNo: string
  readonly reason: string
}): void {
  emit('warn', 'donation.afdian_sponsor_lookup_fallback', {
    out_trade_no: fields.outTradeNo,
    reason: fields.reason,
  })
}

export function logNowPaymentsPartiallyPaid(fields: {
  readonly paymentId: string
  readonly orderId: string | null
  readonly priceCurrency: string
}): void {
  emit('warn', 'donation.nowpayments_partially_paid', {
    payment_id: fields.paymentId,
    order_id: fields.orderId,
    price_currency: fields.priceCurrency,
  })
}

export function logNowPaymentsIntentFallback(fields: NowPaymentsIntentFallbackEvent): void {
  emit('warn', 'donation.nowpayments_intent_fallback', {
    payment_id: fields.paymentId,
    order_id: fields.orderId,
    reason: fields.reason,
  })
}

export function logDonationCommitted(fields: {
  readonly donationId: string
  readonly provider: DonationProvider
  readonly externalId: string
  readonly currency: string
  readonly amountMinor: number
  readonly visibility: string
  readonly outcome: CommitDonationOutcome
  readonly previousVisibility?: NonPrivateVisibilityKind
}): void {
  const payload: Record<string, unknown> = {
    donation_id: fields.donationId,
    provider: fields.provider,
    external_id: fields.externalId,
    currency: fields.currency,
    amount_minor: fields.amountMinor,
    visibility: fields.visibility,
    outcome: fields.outcome,
  }
  if (fields.previousVisibility !== undefined) {
    payload.previous_visibility = fields.previousVisibility
  }
  emit(fields.outcome === 'redacted_private' ? 'warn' : 'info', 'donation.committed', payload)
}

export function logDonationModerated(fields: {
  readonly donationId: string
  readonly action: string
  readonly fromVisibility: string
  readonly toVisibility: string
}): void {
  emit('info', 'donation.moderated', {
    donation_id: fields.donationId,
    action: fields.action,
    from_visibility: fields.fromVisibility,
    to_visibility: fields.toVisibility,
  })
}

export function logAdminManualDonationCreated(fields: {
  readonly donationId: string
  readonly provider: DonationProvider
  readonly externalId: string
  readonly currency: string
  readonly amountMinor: number
  readonly idempotencyKey?: string | null
}): void {
  const payload: Record<string, unknown> = {
    donation_id: fields.donationId,
    provider: fields.provider,
    external_id: fields.externalId,
    currency: fields.currency,
    amount_minor: fields.amountMinor,
  }
  if (fields.idempotencyKey) {
    payload.idempotency_key = fields.idempotencyKey
  }
  emit('info', 'donation.admin_manual_created', payload)
}

export function logAdminManualDonationReplayed(fields: {
  readonly donationId: string
  readonly provider: DonationProvider
  readonly externalId: string
  readonly idempotencyKey: string
}): void {
  emit('info', 'donation.admin_manual_replayed', {
    donation_id: fields.donationId,
    provider: fields.provider,
    external_id: fields.externalId,
    idempotency_key: fields.idempotencyKey,
  })
}

export function logAdminManualIdempotencyConflict(fields: {
  readonly idempotencyKey: string
  readonly existingDonationId: string
}): void {
  emit('warn', 'donation.admin_manual_idempotency_conflict', {
    idempotency_key: fields.idempotencyKey,
    existing_donation_id: fields.existingDonationId,
  })
}

export function logAdminAfdianSynced(fields: {
  readonly fetched: number
  readonly paid: number
  readonly committed: number
  readonly deduplicated: number
  readonly deferred: number
}): void {
  emit('info', 'donation.admin_afdian_synced', {
    fetched: fields.fetched,
    paid: fields.paid,
    committed: fields.committed,
    deduplicated: fields.deduplicated,
    deferred: fields.deferred,
  })
}

export function logAdminConfigUpdated(patch: AdminConfigPatch): void {
  const fields: Record<string, unknown> = {}
  if (patch.minPublicCnyMinor !== undefined) {
    fields.min_public_cny_minor = patch.minPublicCnyMinor
  }
  if (patch.minPublicUsdMinor !== undefined) {
    fields.min_public_usd_minor = patch.minPublicUsdMinor
  }
  if (patch.moderationEnabled !== undefined) {
    fields.moderation_enabled = patch.moderationEnabled
  }
  if (patch.cryptoCheckoutPaused !== undefined) {
    fields.crypto_checkout_paused = patch.cryptoCheckoutPaused
  }
  emit('info', 'donation.admin_config_updated', fields)
}

export function logAdminLogin(fields: {
  readonly ok: boolean
  readonly reason: string
}): void {
  emit(fields.ok ? 'info' : 'warn', 'donation.admin_login', {
    ok: fields.ok,
    reason: fields.reason,
  })
}

export function logAdminSecretRejected(fields: {
  readonly reason: 'too_short'
  readonly minLength: number
}): void {
  emit('warn', 'donation.admin_secret_rejected', {
    reason: fields.reason,
    min_length: fields.minLength,
  })
}

export function logAdminStorageError(fields: {
  readonly method: string
  readonly pathname: string
  readonly message: string
  readonly retryable: boolean
  readonly overloaded: boolean
}): void {
  emit('error', 'donation.admin_storage_error', {
    method: fields.method,
    pathname: fields.pathname,
    message: fields.message,
    retryable: fields.retryable,
    overloaded: fields.overloaded,
  })
}
