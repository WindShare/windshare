import {
  DONATION_PROVIDERS,
  MAX_DISPLAY_NAME_CODEPOINTS,
  MAX_MANUAL_EXTERNAL_ID_CODEPOINTS,
  MAX_MESSAGE_CODEPOINTS,
  type DonationProvider,
} from '../../src/model'
import type { ManualDonationPayload } from './api'
import { renderNotice } from './chrome'
import { h } from './dom'
import { icon } from './icons'
import type { CopyCatalog } from './locales'

const MANUAL_CURRENCIES = [
  'USD',
  'CNY',
  'EUR',
  'GBP',
  'JPY',
  'HKD',
  'TWD',
  'CAD',
  'AUD',
  'SGD',
  'CHF',
] as const

export interface ManualDialogOptions {
  readonly t: CopyCatalog
  readonly onSubmit: (payload: ManualDonationPayload) => Promise<string | null>
  readonly onClose: () => void
}

interface ManualFormControls {
  readonly amountInput: HTMLInputElement
  readonly currencySelect: HTMLSelectElement
  readonly providerSelect: HTMLSelectElement
  readonly donatedAtInput: HTMLInputElement
  readonly nameInput: HTMLInputElement
  readonly messageInput: HTMLTextAreaElement
  readonly externalIdInput: HTMLInputElement
}

function buildCurrencySelect(id: string): HTMLSelectElement {
  const select = h('select', { id })
  for (const code of MANUAL_CURRENCIES) {
    const opt = h('option', { attrs: { value: code } }, code)
    opt.selected = code === 'USD'
    select.appendChild(opt)
  }
  return select
}

function buildProviderSelect(id: string, t: CopyCatalog): HTMLSelectElement {
  const select = h('select', { id })
  // Show 'direct' first as the primary manual-entry channel (e.g. on-chain transfers).
  const ordered: readonly DonationProvider[] = [
    'direct',
    ...DONATION_PROVIDERS.filter(p => p !== 'direct'),
  ]
  for (const provider of ordered) {
    const opt = h('option', { attrs: { value: provider } }, t.providerLabels[provider])
    opt.selected = provider === 'direct'
    select.appendChild(opt)
  }
  return select
}

function attachCodepointCounter(
  input: HTMLInputElement | HTMLTextAreaElement,
  maxCodePoints: number,
): HTMLSpanElement {
  const counter = h('span', { className: 'char-counter' }, `0 / ${maxCodePoints}`)
  const sync = (): void => {
    const length = Array.from(input.value).length
    counter.textContent = `${length} / ${maxCodePoints}`
    counter.classList.toggle('is-over', length > maxCodePoints)
  }
  input.addEventListener('input', sync)
  return counter
}

function buildCounterField<T extends HTMLInputElement | HTMLTextAreaElement>(options: {
  readonly id: string
  readonly label: string
  readonly input: T
  readonly maxCodePoints: number
}): HTMLElement {
  const counter = attachCodepointCounter(options.input, options.maxCodePoints)
  return h(
    'div',
    { className: 'field' },
    h(
      'div',
      { className: 'field-head' },
      h('label', { attrs: { for: options.id } }, options.label),
      counter,
    ),
    options.input,
  )
}

function parseOptionalLocalDateTime(raw: string): { readonly ok: true; readonly value: number | null } | { readonly ok: false } {
  const trimmed = raw.trim()
  if (trimmed.length === 0) return { ok: true, value: null }
  const ms = Date.parse(trimmed)
  if (!Number.isFinite(ms) || ms <= 0) return { ok: false }
  return { ok: true, value: Math.floor(ms / 1000) }
}

function optionalTrimmed(raw: string): string | null {
  const trimmed = raw.trim()
  return trimmed.length > 0 ? trimmed : null
}

function collectManualPayload(
  controls: ManualFormControls,
  t: CopyCatalog,
):
  | { readonly ok: true; readonly payload: ManualDonationPayload }
  | { readonly ok: false; readonly message: string; readonly focusEl: HTMLElement } {
  const amount = controls.amountInput.value.trim()
  if (amount.length === 0) {
    return { ok: false, message: t.errors.requiredAmount, focusEl: controls.amountInput }
  }
  if (Array.from(controls.nameInput.value).length > MAX_DISPLAY_NAME_CODEPOINTS) {
    return { ok: false, message: t.errors.invalidManualDonation, focusEl: controls.nameInput }
  }
  if (Array.from(controls.messageInput.value).length > MAX_MESSAGE_CODEPOINTS) {
    return { ok: false, message: t.errors.invalidManualDonation, focusEl: controls.messageInput }
  }
  if (Array.from(controls.externalIdInput.value).length > MAX_MANUAL_EXTERNAL_ID_CODEPOINTS) {
    return { ok: false, message: t.errors.invalidManualDonation, focusEl: controls.externalIdInput }
  }
  const donatedAt = parseOptionalLocalDateTime(controls.donatedAtInput.value)
  if (!donatedAt.ok) {
    return { ok: false, message: t.errors.invalidManualDonation, focusEl: controls.donatedAtInput }
  }
  return {
    ok: true,
    payload: {
      provider: controls.providerSelect.value as DonationProvider,
      amount,
      currency: controls.currencySelect.value,
      displayName: optionalTrimmed(controls.nameInput.value),
      message: optionalTrimmed(controls.messageInput.value),
      externalId: optionalTrimmed(controls.externalIdInput.value),
      donatedAt: donatedAt.value,
    },
  }
}

function buildManualFormControls(t: CopyCatalog): ManualFormControls {
  return {
    amountInput: h('input', {
      id: 'manual-amount-input',
      attrs: {
        type: 'text',
        inputmode: 'decimal',
        placeholder: t.manualAmountPlaceholder,
        autocomplete: 'off',
      },
    }),
    currencySelect: buildCurrencySelect('manual-currency-select'),
    providerSelect: buildProviderSelect('manual-provider-select', t),
    donatedAtInput: h('input', {
      id: 'manual-donated-at-input',
      attrs: { type: 'datetime-local' },
    }),
    nameInput: h('input', {
      id: 'manual-name-input',
      attrs: {
        type: 'text',
        maxlength: String(MAX_DISPLAY_NAME_CODEPOINTS),
        placeholder: t.manualNamePlaceholder,
        autocomplete: 'off',
      },
    }),
    messageInput: h('textarea', {
      id: 'manual-message-input',
      attrs: {
        rows: '3',
        maxlength: String(MAX_MESSAGE_CODEPOINTS),
        placeholder: t.manualMessagePlaceholder,
      },
    }),
    externalIdInput: h('input', {
      id: 'manual-external-id-input',
      attrs: {
        type: 'text',
        maxlength: String(MAX_MANUAL_EXTERNAL_ID_CODEPOINTS),
        placeholder: t.manualExternalIdPlaceholder,
        autocomplete: 'off',
      },
    }),
  }
}

export function openManualDonationDialog(options: ManualDialogOptions): HTMLDialogElement {
  const { t, onSubmit, onClose } = options
  let submitting = false

  const controls = buildManualFormControls(t)
  const errorSlot = h('div')
  const cancelBtn = h('button', { className: 'secondary', attrs: { type: 'button' } }, t.cancel)
  const submitBtn = h('button', { attrs: { type: 'submit' } }, t.confirmAddDonation)

  const lockableControls: readonly (HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement | HTMLButtonElement)[] = [
    controls.amountInput,
    controls.currencySelect,
    controls.providerSelect,
    controls.donatedAtInput,
    controls.nameInput,
    controls.messageInput,
    controls.externalIdInput,
    cancelBtn,
    submitBtn,
  ]

  const setBusy = (busy: boolean): void => {
    submitting = busy
    for (const el of lockableControls) el.disabled = busy
    submitBtn.replaceChildren(
      ...(busy ? [icon('loader', 15), t.addingDonation] : [t.confirmAddDonation]),
    )
  }

  const showError = (message: string, focusEl?: HTMLElement): void => {
    const notice = renderNotice({ kind: 'error', format: () => message }, t)
    if (notice !== null) errorSlot.replaceChildren(notice)
    focusEl?.focus()
  }

  const closeCleanly = (): void => {
    if (submitting) return
    dialog.close()
    dialog.remove()
    onClose()
  }

  cancelBtn.addEventListener('click', closeCleanly)

  const clearError = (): void => {
    if (errorSlot.childElementCount > 0) {
      errorSlot.replaceChildren()
    }
  }

  const form = h(
    'form',
    {
      className: 'dialog-form',
      on: {
        input: clearError,
        change: clearError,
        submit: async event => {
          event.preventDefault()
          if (submitting) return
          errorSlot.replaceChildren()
          const collected = collectManualPayload(controls, t)
          if (!collected.ok) {
            showError(collected.message, collected.focusEl)
            return
          }
          setBusy(true)
          const errorMessage = await onSubmit(collected.payload)
          if (errorMessage === null) {
            dialog.close()
            dialog.remove()
            onClose()
            return
          }
          setBusy(false)
          showError(errorMessage, controls.amountInput)
        },
      },
    },
    h(
      'div',
      { className: 'dialog-row amount-currency' },
      h(
        'div',
        { className: 'field' },
        h('label', { attrs: { for: 'manual-amount-input' } }, t.manualAmountLabel),
        controls.amountInput,
      ),
      h(
        'div',
        { className: 'field' },
        h('label', { attrs: { for: 'manual-currency-select' } }, t.manualCurrencyLabel),
        controls.currencySelect,
      ),
    ),
    h(
      'div',
      { className: 'dialog-row two-col' },
      h(
        'div',
        { className: 'field' },
        h('label', { attrs: { for: 'manual-provider-select' } }, t.manualProviderLabel),
        controls.providerSelect,
      ),
      h(
        'div',
        { className: 'field' },
        h('label', { attrs: { for: 'manual-donated-at-input' } }, t.manualDonatedAtLabel),
        controls.donatedAtInput,
      ),
    ),
    buildCounterField({
      id: 'manual-name-input',
      label: t.manualNameLabel,
      input: controls.nameInput,
      maxCodePoints: MAX_DISPLAY_NAME_CODEPOINTS,
    }),
    buildCounterField({
      id: 'manual-message-input',
      label: t.manualMessageLabel,
      input: controls.messageInput,
      maxCodePoints: MAX_MESSAGE_CODEPOINTS,
    }),
    buildCounterField({
      id: 'manual-external-id-input',
      label: t.manualExternalIdLabel,
      input: controls.externalIdInput,
      maxCodePoints: MAX_MANUAL_EXTERNAL_ID_CODEPOINTS,
    }),
    errorSlot,
    h('div', { className: 'dialog-actions' }, cancelBtn, submitBtn),
  )

  const dialog = h(
    'dialog',
    {
      className: 'manual-dialog',
      attrs: { 'aria-labelledby': 'manual-dialog-heading' },
      on: {
        cancel: event => {
          event.preventDefault()
          closeCleanly()
        },
        click: event => {
          if (event.target === dialog) closeCleanly()
        },
      },
    },
    h('h2', { id: 'manual-dialog-heading' }, t.manualDialogTitle),
    form,
  )

  document.body.appendChild(dialog)
  dialog.showModal()
  controls.amountInput.focus()
  return dialog
}
