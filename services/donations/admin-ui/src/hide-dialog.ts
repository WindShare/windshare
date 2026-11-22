import { MAX_HIDE_REASON_CODEPOINTS, type DonationRecord } from '../../src/model'
import { renderNotice } from './chrome'
import { h } from './dom'
import { formatDonationMoney } from './format'
import { icon } from './icons'
import type { CopyCatalog } from './locales'

export interface HideDialogOptions {
  readonly record: DonationRecord
  readonly t: CopyCatalog
  readonly onSubmit: (reason: string) => Promise<string | null>
  readonly onClose: () => void
}

function createReasonPreview(record: DonationRecord, t: CopyCatalog): HTMLElement {
  const donor = record.displayName ?? t.anonymousDonor
  return h(
    'div',
    { className: 'dialog-preview' },
    h(
      'div',
      { className: 'dialog-preview-head' },
      h('strong', null, donor),
      h('span', { className: 'amount-pill' }, formatDonationMoney(record)),
    ),
    record.message === null
      ? null
      : h('p', { className: 'dialog-preview-msg' }, record.message),
  )
}

export function openHideDialog(options: HideDialogOptions): HTMLDialogElement {
  const { record, t, onSubmit, onClose } = options
  let submitting = false

  const counter = h('span', { className: 'char-counter' }, `0 / ${MAX_HIDE_REASON_CODEPOINTS}`)
  const errorSlot = h('div')
  const input = h('input', {
    id: 'hide-reason-input',
    attrs: {
      type: 'text',
      maxlength: String(MAX_HIDE_REASON_CODEPOINTS),
      placeholder: t.hideReasonPlaceholder,
      autocomplete: 'off',
    },
  })

  const chipButtons: HTMLButtonElement[] = []
  const syncInputMeta = (): void => {
    const length = Array.from(input.value).length
    counter.textContent = `${length} / ${MAX_HIDE_REASON_CODEPOINTS}`
    const current = input.value.trim()
    for (const chip of chipButtons) {
      chip.setAttribute('aria-pressed', String(chip.textContent === current))
    }
  }

  input.addEventListener('input', syncInputMeta)

  const chipsWrap = h('div', { className: 'preset-chips' })
  for (const preset of t.hidePresets) {
    const chip = h(
      'button',
      {
        className: 'preset-chip',
        attrs: { type: 'button', 'aria-pressed': 'false' },
        on: {
          click: () => {
            input.value = input.value.trim() === preset ? '' : preset
            syncInputMeta()
            input.focus()
          },
        },
      },
      preset,
    )
    chipButtons.push(chip)
    chipsWrap.appendChild(chip)
  }

  const cancelBtn = h(
    'button',
    { className: 'secondary', attrs: { type: 'button' } },
    t.cancel,
  )
  const confirmBtn = h(
    'button',
    { className: 'destructive', attrs: { type: 'submit' } },
    t.confirmHide,
  )

  const closeCleanly = (): void => {
    if (submitting) return
    dialog.close()
    dialog.remove()
    onClose()
  }

  cancelBtn.addEventListener('click', closeCleanly)

  const form = h(
    'form',
    {
      className: 'dialog-form',
      on: {
        submit: async event => {
          event.preventDefault()
          if (submitting) return
          submitting = true
          errorSlot.replaceChildren()
          input.disabled = true
          cancelBtn.disabled = true
          confirmBtn.disabled = true
          confirmBtn.replaceChildren(icon('loader', 15), t.hiding)
          for (const chip of chipButtons) chip.disabled = true

          const reason = input.value.trim() || 'moderated'
          const errorMessage = await onSubmit(reason)
          if (errorMessage === null) {
            dialog.close()
            dialog.remove()
            onClose()
            return
          }

          submitting = false
          input.disabled = false
          cancelBtn.disabled = false
          confirmBtn.disabled = false
          confirmBtn.replaceChildren(t.confirmHide)
          for (const chip of chipButtons) chip.disabled = false
          const notice = renderNotice({ kind: 'error', format: () => errorMessage }, t)
          if (notice !== null) errorSlot.replaceChildren(notice)
          input.focus()
        },
      },
    },
    h(
      'div',
      { className: 'field' },
      h(
        'div',
        { className: 'field-head' },
        h('label', { attrs: { for: 'hide-reason-input' } }, t.hideReasonLabel),
        counter,
      ),
      input,
    ),
    errorSlot,
    h('div', { className: 'dialog-actions' }, cancelBtn, confirmBtn),
  )

  const dialog = h(
    'dialog',
    {
      attrs: { 'aria-labelledby': 'hide-dialog-heading' },
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
    h('h2', { id: 'hide-dialog-heading' }, t.hideDialogTitle),
    createReasonPreview(record, t),
    chipsWrap,
    form,
  )

  document.body.appendChild(dialog)
  dialog.showModal()
  input.focus()
  return dialog
}
