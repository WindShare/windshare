import type { AdminConfigPatch, DonationConfig } from '../../src/model'
import { renderNotice, type FeedbackNotice } from './chrome'
import { h } from './dom'
import { formatMinorDecimal, parseMajorTwoDecimalToMinor } from './format'
import { icon } from './icons'
import type { CopyCatalog } from './locales'

export interface SettingsDraft {
  readonly cnyText: string
  readonly usdText: string
  readonly moderationEnabled: boolean
  readonly cryptoCheckoutPaused: boolean
}

export interface SettingsViewModel {
  readonly t: CopyCatalog
  readonly savedConfig: DonationConfig
  readonly draft: SettingsDraft
  readonly saving: boolean
  readonly feedback: FeedbackNotice | null
}

export interface SettingsViewCallbacks {
  readonly onDraftChange: (draft: SettingsDraft) => void
  readonly onDiscard: () => void
  readonly onSave: (patch: AdminConfigPatch) => void
  readonly onDismissFeedback: () => void
}

export interface SettingsViewHandle {
  readonly element: HTMLElement
  update(model: SettingsViewModel): void
}

export function configToDraft(config: DonationConfig): SettingsDraft {
  return {
    cnyText: formatMinorDecimal(config.minPublicCnyMinor, 2),
    usdText: formatMinorDecimal(config.minPublicUsdMinor, 2),
    moderationEnabled: config.moderationEnabled,
    cryptoCheckoutPaused: config.cryptoCheckoutPaused,
  }
}

export function buildConfigPatch(
  saved: DonationConfig,
  draft: SettingsDraft,
): { readonly valid: false } | { readonly valid: true; readonly patch: AdminConfigPatch; readonly dirty: boolean } {
  const cnyMinor = parseMajorTwoDecimalToMinor(draft.cnyText)
  const usdMinor = parseMajorTwoDecimalToMinor(draft.usdText)
  if (cnyMinor === null || usdMinor === null) {
    return { valid: false }
  }

  const patch: {
    minPublicCnyMinor?: number
    minPublicUsdMinor?: number
    moderationEnabled?: boolean
    cryptoCheckoutPaused?: boolean
  } = {}
  let dirty = false

  if (cnyMinor !== saved.minPublicCnyMinor) {
    patch.minPublicCnyMinor = cnyMinor
    dirty = true
  }
  if (usdMinor !== saved.minPublicUsdMinor) {
    patch.minPublicUsdMinor = usdMinor
    dirty = true
  }
  if (draft.moderationEnabled !== saved.moderationEnabled) {
    patch.moderationEnabled = draft.moderationEnabled
    dirty = true
  }
  if (draft.cryptoCheckoutPaused !== saved.cryptoCheckoutPaused) {
    patch.cryptoCheckoutPaused = draft.cryptoCheckoutPaused
    dirty = true
  }

  return { valid: true, patch, dirty }
}

function createMoneyField(id: string, symbol: string, code: string): {
  readonly root: HTMLElement
  readonly label: HTMLLabelElement
  readonly box: HTMLElement
  readonly input: HTMLInputElement
  readonly error: HTMLElement
} {
  const label = h('label', { attrs: { for: id } })
  const input = h('input', {
    id,
    attrs: {
      type: 'text',
      inputmode: 'decimal',
      autocomplete: 'off',
      spellcheck: 'false',
    },
  })
  const box = h(
    'div',
    { className: 'money-input' },
    h('span', { className: 'money-adorn' }, symbol),
    input,
    h('span', { className: 'money-adorn code' }, code),
  )
  const error = h('span', { className: 'field-error', attrs: { hidden: true } })
  return {
    root: h('div', { className: 'field' }, label, box, error),
    label,
    box,
    input,
    error,
  }
}

function createSwitchRow(id: string, warnTone: boolean): {
  readonly row: HTMLLabelElement
  readonly title: HTMLSpanElement
  readonly badge: HTMLSpanElement
  readonly input: HTMLInputElement
} {
  const title = h('span')
  const badge = h('span', { className: 'badge' })
  const input = h('input', {
    id,
    className: warnTone ? 'switch warn' : 'switch',
    attrs: { type: 'checkbox', role: 'switch' },
  })
  const row = h(
    'label',
    { className: 'switch-row', attrs: { for: id } },
    h('span', { className: 'switch-row-title' }, title, badge),
    input,
  )
  return { row, title, badge, input }
}

export function createSettingsView(
  initial: SettingsViewModel,
  callbacks: SettingsViewCallbacks,
): SettingsViewHandle {
  let currentModel = initial

  const heading = h('h1')
  const thresholdsHeading = h('h2')
  const controlsHeading = h('h2')
  const cnyField = createMoneyField('cfg-cny-major', '¥', 'CNY')
  const usdField = createMoneyField('cfg-usd-major', '$', 'USD')
  const moderationRow = createSwitchRow('cfg-moderation', false)
  const cryptoRow = createSwitchRow('cfg-crypto-paused', true)
  const noticeSlot = h('div')
  const saveBtn = h('button', { attrs: { type: 'submit' } })
  const discardBtn = h('button', {
    className: 'quiet',
    attrs: { type: 'button' },
    on: { click: callbacks.onDiscard },
  })

  const emitDraft = (): void => {
    callbacks.onDraftChange({
      cnyText: cnyField.input.value,
      usdText: usdField.input.value,
      moderationEnabled: moderationRow.input.checked,
      cryptoCheckoutPaused: cryptoRow.input.checked,
    })
  }

  cnyField.input.addEventListener('input', emitDraft)
  usdField.input.addEventListener('input', emitDraft)
  moderationRow.input.addEventListener('change', emitDraft)
  cryptoRow.input.addEventListener('change', emitDraft)

  const form = h(
    'form',
    {
      className: 'settings-form',
      attrs: { novalidate: true },
      on: {
        submit: event => {
          event.preventDefault()
          const evaluated = buildConfigPatch(currentModel.savedConfig, currentModel.draft)
          if (!evaluated.valid || !evaluated.dirty) return
          callbacks.onSave(evaluated.patch)
        },
      },
    },
    h(
      'div',
      { className: 'settings-grid' },
      h(
        'section',
        { className: 'settings-section' },
        thresholdsHeading,
        h('div', { className: 'threshold-stack' }, cnyField.root, usdField.root),
      ),
      h(
        'section',
        { className: 'settings-section' },
        controlsHeading,
        moderationRow.row,
        cryptoRow.row,
      ),
    ),
    noticeSlot,
    h('div', { className: 'settings-actions' }, saveBtn, discardBtn),
  )

  const root = h(
    'div',
    { className: 'view' },
    h('div', { className: 'page-title' }, heading),
    h('section', { className: 'panel padded' }, form),
  )

  const update = (model: SettingsViewModel): void => {
    currentModel = model
    const { t, savedConfig, draft, saving, feedback } = model

    heading.textContent = t.settingsTitle
    thresholdsHeading.textContent = t.sectionThresholds
    controlsHeading.textContent = t.sectionControls
    cnyField.label.textContent = t.minCnyLabel
    usdField.label.textContent = t.minUsdLabel

    if (document.activeElement !== cnyField.input && cnyField.input.value !== draft.cnyText) {
      cnyField.input.value = draft.cnyText
    }
    if (document.activeElement !== usdField.input && usdField.input.value !== draft.usdText) {
      usdField.input.value = draft.usdText
    }

    const cnyInvalid = parseMajorTwoDecimalToMinor(draft.cnyText) === null
    const usdInvalid = parseMajorTwoDecimalToMinor(draft.usdText) === null
    cnyField.box.classList.toggle('invalid', cnyInvalid)
    cnyField.input.setAttribute('aria-invalid', String(cnyInvalid))
    cnyField.error.textContent = t.invalidThreshold
    cnyField.error.hidden = !cnyInvalid

    usdField.box.classList.toggle('invalid', usdInvalid)
    usdField.input.setAttribute('aria-invalid', String(usdInvalid))
    usdField.error.textContent = t.invalidThreshold
    usdField.error.hidden = !usdInvalid

    moderationRow.title.textContent = t.moderationLabel
    moderationRow.input.checked = draft.moderationEnabled
    moderationRow.badge.className = draft.moderationEnabled ? 'badge warning' : 'badge'
    moderationRow.badge.textContent = draft.moderationEnabled ? t.switchOn : t.switchOff

    cryptoRow.title.textContent = t.cryptoPauseLabel
    cryptoRow.input.checked = draft.cryptoCheckoutPaused
    cryptoRow.badge.className = draft.cryptoCheckoutPaused ? 'badge warning' : 'badge good'
    cryptoRow.badge.textContent = draft.cryptoCheckoutPaused ? t.checkoutPaused : t.checkoutActive

    cnyField.input.disabled = saving
    usdField.input.disabled = saving
    moderationRow.input.disabled = saving
    cryptoRow.input.disabled = saving

    const evaluated = buildConfigPatch(savedConfig, draft)
    const dirty = !evaluated.valid || evaluated.dirty
    saveBtn.disabled = saving || !evaluated.valid || !evaluated.dirty
    saveBtn.replaceChildren(
      ...(saving ? [icon('loader', 15), t.savingSettings] : [t.saveSettings]),
    )

    discardBtn.textContent = t.discard
    discardBtn.disabled = saving
    discardBtn.hidden = !dirty

    const noticeNode = renderNotice(feedback, t, callbacks.onDismissFeedback)
    noticeSlot.replaceChildren(...(noticeNode === null ? [] : [noticeNode]))
    noticeSlot.hidden = noticeNode === null
  }

  update(initial)
  return { element: root, update }
}
