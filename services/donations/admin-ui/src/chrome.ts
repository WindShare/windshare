import { h } from './dom'
import { icon, windMark, type IconName } from './icons'
import type { CopyCatalog } from './locales'
import {
  APPEARANCES,
  LANGUAGE_NAMES,
  SUPPORTED_LOCALES,
  isLocale,
  parseAppearance,
  type Appearance,
  type Locale,
} from './preferences'

export type NoticeKind = 'success' | 'warning' | 'error'
export type LocalizedText = (t: CopyCatalog) => string

export interface FeedbackNotice {
  readonly kind: NoticeKind
  readonly format: LocalizedText
}

const NOTICE_ICONS: Readonly<Record<NoticeKind, IconName>> = {
  success: 'check',
  warning: 'clock',
  error: 'alert',
}

export interface MastheadOptions {
  readonly locale: Locale
  readonly appearance: Appearance
  readonly authenticated: boolean
  readonly signingOut: boolean
  readonly t: CopyCatalog
  readonly onSelectLocale: (locale: Locale) => void
  readonly onSelectAppearance: (appearance: Appearance) => void
  readonly onSignOut: () => void
}

function buildLanguageControl(options: MastheadOptions): HTMLElement {
  const { locale, t, onSelectLocale } = options
  const select = h('select', {
    attrs: { 'aria-label': t.language },
    on: {
      change: event => {
        const value = (event.currentTarget as HTMLSelectElement).value
        if (isLocale(value)) onSelectLocale(value)
      },
    },
  })
  for (const code of SUPPORTED_LOCALES) {
    const opt = h('option', { attrs: { value: code, lang: code } }, LANGUAGE_NAMES[code])
    opt.selected = code === locale
    select.appendChild(opt)
  }
  return h(
    'label',
    { className: 'preference language', title: t.language },
    icon('globe', 16),
    h('span', { attrs: { 'aria-hidden': 'true' } }, LANGUAGE_NAMES[locale]),
    icon('down', 13),
    select,
  )
}

function buildAppearanceControl(options: MastheadOptions): HTMLElement {
  const { appearance, t, onSelectAppearance } = options
  const select = h('select', {
    attrs: { 'aria-label': t.appearance },
    on: {
      change: event => {
        onSelectAppearance(parseAppearance((event.currentTarget as HTMLSelectElement).value))
      },
    },
  })
  for (const mode of APPEARANCES) {
    const opt = h('option', { attrs: { value: mode } }, t.appearanceOptions[mode])
    opt.selected = mode === appearance
    select.appendChild(opt)
  }
  return h(
    'label',
    {
      className: 'preference appearance',
      title: `${t.appearance} · ${t.appearanceOptions[appearance]}`,
    },
    icon(appearance, 17),
    h('span', { className: 'sr-only' }, t.appearanceOptions[appearance]),
    select,
  )
}

export function renderMasthead(options: MastheadOptions): HTMLElement {
  const { authenticated, signingOut, t, onSignOut } = options
  const signOutIcon = icon(signingOut ? 'loader' : 'logout', 16)
  const accountTools = authenticated
    ? h(
      'div',
      { className: 'mast-account' },
      h(
        'button',
        {
          className: 'quiet',
          title: t.signOut,
          attrs: {
            type: 'button',
            'aria-label': t.signOut,
            disabled: signingOut,
          },
          on: { click: onSignOut },
        },
        signOutIcon,
        h('span', null, t.signOut),
      ),
    )
    : null

  return h(
    'header',
    { className: 'masthead' },
    h(
      'div',
      { className: 'mast-inner' },
      h(
        'div',
        { className: 'brand' },
        windMark(),
        h(
          'div',
          { className: 'brand-type' },
          h('span', { className: 'wordmark' }, 'WindShare'),
          h('span', { className: 'area' }, t.areaTitle),
        ),
      ),
      h(
        'div',
        { className: 'mast-tools' },
        buildLanguageControl(options),
        buildAppearanceControl(options),
        accountTools,
      ),
    ),
  )
}

export interface WorkspaceBarOptions {
  readonly activeTab: 'donations' | 'settings'
  readonly pendingCount: number
  readonly settingsAttention: boolean
  readonly afdianSyncConfigured: boolean
  readonly syncing: boolean
  readonly refreshing: boolean
  readonly t: CopyCatalog
  readonly onOpenManualDialog: () => void
  readonly onSyncAfdian: () => void
  readonly onRefresh: () => void
}

export function renderWorkspaceBar(options: WorkspaceBarOptions): HTMLElement {
  const {
    activeTab,
    pendingCount,
    settingsAttention,
    afdianSyncConfigured,
    syncing,
    refreshing,
    t,
    onOpenManualDialog,
    onSyncAfdian,
    onRefresh,
  } = options

  const donationsLink = h(
    'a',
    {
      attrs: {
        href: '#donations',
        'aria-current': activeTab === 'donations' ? 'page' : undefined,
      },
    },
    icon('heart', 16),
    t.navDonations,
    pendingCount > 0 ? h('span', { className: 'nav-pill-count' }, String(pendingCount)) : null,
  )

  const settingsLink = h(
    'a',
    {
      attrs: {
        href: '#settings',
        'aria-current': activeTab === 'settings' ? 'page' : undefined,
      },
    },
    icon('sliders', 16),
    t.navSettings,
    settingsAttention ? h('span', { className: 'nav-dot', attrs: { 'aria-hidden': 'true' } }) : null,
  )

  const addBtn = h(
    'button',
    {
      title: t.addDonation,
      attrs: {
        type: 'button',
        'aria-label': t.addDonation,
      },
      on: { click: onOpenManualDialog },
    },
    icon('plus', 16),
    h('span', { className: 'action-text' }, t.addDonation),
  )

  const syncTitle = afdianSyncConfigured ? t.syncAfdian : t.syncNotConfigured
  const syncBtn = h(
    'button',
    {
      className: 'secondary',
      title: syncTitle,
      attrs: {
        type: 'button',
        'aria-label': syncTitle,
        disabled: !afdianSyncConfigured || syncing || refreshing,
      },
      on: { click: onSyncAfdian },
    },
    icon(syncing ? 'loader' : 'sync', 16),
    h('span', { className: 'action-text' }, syncing ? t.syncing : t.syncAfdian),
  )

  const refreshLabel = refreshing ? t.refreshing : t.refresh
  const refreshBtn = h(
    'button',
    {
      className: 'quiet',
      title: refreshLabel,
      attrs: {
        type: 'button',
        'aria-label': refreshLabel,
        disabled: refreshing || syncing,
      },
      on: { click: onRefresh },
    },
    icon(refreshing ? 'loader' : 'refresh', 16),
    h('span', { className: 'action-text' }, refreshLabel),
  )

  return h(
    'div',
    { className: 'workspace-bar' },
    h('nav', { className: 'workspace-nav', attrs: { 'aria-label': t.navLabel } }, donationsLink, settingsLink),
    h('div', { className: 'workspace-actions' }, addBtn, syncBtn, refreshBtn),
  )
}

export function renderNotice(
  feedback: FeedbackNotice | null,
  t: CopyCatalog,
  onDismiss?: () => void,
): HTMLElement | null {
  if (feedback === null) return null
  return h(
    'div',
    {
      className: `notice ${feedback.kind}`,
      role: feedback.kind === 'error' ? 'alert' : 'status',
    },
    icon(NOTICE_ICONS[feedback.kind], 17),
    h('span', null, feedback.format(t)),
    onDismiss === undefined
      ? null
      : h(
        'button',
        {
          className: 'quiet',
          title: t.dismiss,
          attrs: { type: 'button', 'aria-label': t.dismiss },
          on: { click: onDismiss },
        },
        icon('close', 15),
      ),
  )
}

export function renderReadError(
  error: LocalizedText | null,
  t: CopyCatalog,
  onRetry: () => void,
): HTMLElement | null {
  if (error === null) return null
  return h(
    'div',
    { className: 'read-error' },
    icon('alert', 17),
    h('span', { role: 'alert' }, error(t)),
    h('button', { className: 'quiet', attrs: { type: 'button' }, on: { click: onRetry } }, t.retry),
  )
}

export function renderSessionWait(
  error: LocalizedText | null,
  t: CopyCatalog,
  onRetry: () => void,
): HTMLElement {
  const isError = error !== null
  return h(
    'div',
    { className: 'focus-page' },
    h(
      'section',
      { className: 'focus-card' },
      h(
        'header',
        { className: 'focus-heading' },
        h(
          'div',
          { className: 'focus-context' },
          h('span', { className: 'context-icon' }, icon(isError ? 'alert' : 'loader', 19)),
          t.adminRole,
        ),
        h('h1', null, isError ? t.openFailed : t.opening),
      ),
      h('div', { className: 'focus-body' }, renderReadError(error, t, onRetry)),
    ),
  )
}
