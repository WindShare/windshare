import { renderNotice, type FeedbackNotice } from './chrome'
import { h } from './dom'
import { entryArtwork, icon } from './icons'
import type { CopyCatalog } from './locales'

export interface LoginViewOptions {
  readonly t: CopyCatalog
  readonly busy: boolean
  readonly feedback: FeedbackNotice | null
  readonly onSubmit: (password: string) => void
}

export function renderLoginView(options: LoginViewOptions): HTMLElement {
  const { t, busy, feedback, onSubmit } = options
  let visible = false

  const input = h('input', {
    id: 'admin-password',
    attrs: {
      name: 'password',
      type: 'password',
      autocomplete: 'current-password',
      required: true,
      disabled: busy,
    },
  })

  const revealBtn = h(
    'button',
    {
      className: 'quiet reveal',
      title: t.showPassword,
      attrs: { type: 'button', 'aria-label': t.showPassword, disabled: busy },
    },
    icon('eye', 18),
  )

  revealBtn.addEventListener('click', () => {
    visible = !visible
    input.type = visible ? 'text' : 'password'
    const label = visible ? t.hidePassword : t.showPassword
    revealBtn.title = label
    revealBtn.setAttribute('aria-label', label)
    revealBtn.replaceChildren(icon(visible ? 'eyeOff' : 'eye', 18))
  })

  const submitBtn = h(
    'button',
    { attrs: { type: 'submit', disabled: busy } },
    busy ? t.signingIn : t.signIn,
    icon(busy ? 'loader' : 'arrow', 16),
  )

  const hiddenUser = h('input', {
    attrs: {
      type: 'text',
      name: 'username',
      value: 'admin',
      autocomplete: 'username',
      tabindex: '-1',
      hidden: true,
    },
  })

  const form = h(
    'form',
    {
      className: 'stack',
      attrs: { novalidate: true },
      on: {
        submit: event => {
          event.preventDefault()
          onSubmit(input.value)
        },
      },
    },
    hiddenUser,
    h(
      'div',
      { className: 'field' },
      h('label', { attrs: { for: 'admin-password' } }, t.password),
      h('div', { className: 'password' }, input, revealBtn),
    ),
    renderNotice(feedback, t),
    submitBtn,
  )

  const hostBadge = h(
    'div',
    { className: 'map-host' },
    icon('globe', 15),
    h('bdi', null, window.location.host),
  )

  return h(
    'div',
    { className: 'focus-page' },
    h(
      'section',
      { className: 'entry-card' },
      h('div', { className: 'entry-map' }, entryArtwork(), hostBadge),
      h(
        'div',
        { className: 'entry-content' },
        h(
          'header',
          { className: 'focus-heading' },
          h(
            'div',
            { className: 'focus-context' },
            h('span', { className: 'context-icon' }, icon('shield', 19)),
            t.adminRole,
          ),
          h('h1', null, t.signIn),
        ),
        h('div', { className: 'focus-body' }, form),
      ),
    ),
  )
}
