import {
  DONATION_PROVIDERS,
  type AdminVisibilityCounts,
  type DonationConfig,
  type DonationProvider,
  type DonationRecord,
  type VisibilityKind,
} from '../../src/model'
import { renderNotice, renderReadError, type FeedbackNotice, type LocalizedText } from './chrome'
import { h } from './dom'
import { formatCount, formatDateTime, formatDonationMoney } from './format'
import { icon, type IconName } from './icons'
import type { CopyCatalog } from './locales'
import type { Locale } from './preferences'

export type VisibilityFilter = 'all' | VisibilityKind
export type ProviderFilter = 'all' | DonationProvider

export interface DonationsViewModel {
  readonly locale: Locale
  readonly t: CopyCatalog
  readonly items: readonly DonationRecord[]
  readonly counts: AdminVisibilityCounts
  readonly config: DonationConfig
  readonly nextCursor: string | null
  readonly visibilityFilter: VisibilityFilter
  readonly providerFilter: ProviderFilter
  readonly searchQuery: string
  readonly loading: boolean
  readonly loadingMore: boolean
  readonly busyIds: ReadonlySet<string>
  readonly afdianSyncConfigured: boolean
  readonly loadError: LocalizedText | null
  readonly notice: FeedbackNotice | null
}

export interface DonationsViewCallbacks {
  readonly onSelectVisibility: (filter: VisibilityFilter) => void
  readonly onSelectProvider: (provider: ProviderFilter) => void
  readonly onSearchChange: (query: string) => void
  readonly onResetFilters: () => void
  readonly onRetryLoad: () => void
  readonly onLoadMore: () => void
  readonly onOpenManualDialog: () => void
  readonly onSyncAfdian: () => void
  readonly onApprove: (record: DonationRecord) => void
  readonly onRequestHide: (record: DonationRecord) => void
  readonly onRestore: (record: DonationRecord) => void
  readonly onDismissNotice: () => void
}

const FILTER_ORDER: readonly VisibilityFilter[] = [
  'all',
  'pending_review',
  'published',
  'below_threshold',
  'hidden',
  'private_donor',
]

function filterCountValue(counts: AdminVisibilityCounts, filter: VisibilityFilter): number {
  return filter === 'all' ? counts.total : counts[filter]
}

function statusBadgeTone(kind: VisibilityKind): string {
  switch (kind) {
    case 'published':
      return 'badge good'
    case 'pending_review':
      return 'badge warning'
    case 'hidden':
      return 'badge danger'
    case 'private_donor':
      return 'badge info'
    case 'below_threshold':
      return 'badge'
  }
}

function renderStatusPills(config: DonationConfig, t: CopyCatalog): HTMLElement | null {
  if (!config.moderationEnabled && !config.cryptoCheckoutPaused) return null
  return h(
    'div',
    { className: 'status-pills' },
    config.moderationEnabled
      ? h('a', { className: 'badge warning badge-link', attrs: { href: '#settings' } }, t.preReviewOn)
      : null,
    config.cryptoCheckoutPaused
      ? h('a', { className: 'badge warning badge-link', attrs: { href: '#settings' } }, t.cryptoPaused)
      : null,
  )
}

function renderOverviewStrip(
  model: DonationsViewModel,
  onSelect: (filter: VisibilityFilter) => void,
): HTMLElement {
  const { counts, visibilityFilter, locale, t } = model
  const tiles: readonly {
    readonly filter: VisibilityFilter
    readonly label: string
    readonly value: number
    readonly iconName: IconName
    readonly iconClass: string
    readonly attention?: boolean
  }[] = [
    { filter: 'all', label: t.metricTotal, value: counts.total, iconName: 'heart', iconClass: 'metric-icon' },
    {
      filter: 'pending_review',
      label: t.metricPending,
      value: counts.pending_review,
      iconName: 'clock',
      iconClass: 'metric-icon pending',
      attention: counts.pending_review > 0,
    },
    { filter: 'published', label: t.metricPublished, value: counts.published, iconName: 'check', iconClass: 'metric-icon published' },
    { filter: 'hidden', label: t.metricHidden, value: counts.hidden, iconName: 'eyeOff', iconClass: 'metric-icon hidden-tone' },
  ]

  return h(
    'section',
    { className: 'panel overview-strip', attrs: { 'aria-label': t.navDonations } },
    ...tiles.map(tile => h(
      'button',
      {
        className: tile.attention ? 'overview-tile attention' : 'overview-tile',
        attrs: {
          type: 'button',
          'aria-pressed': String(visibilityFilter === tile.filter),
        },
        on: { click: () => onSelect(tile.filter) },
      },
      h(
        'span',
        { className: 'overview-label' },
        h('span', { className: tile.iconClass }, icon(tile.iconName, 16)),
        tile.label,
      ),
      h('span', { className: 'overview-value' }, formatCount(tile.value, locale)),
    )),
  )
}

function renderCopyOrderButton(externalId: string, t: CopyCatalog): HTMLButtonElement {
  const label = h('span', null, externalId)
  const iconWrap = h('span', null, icon('copy', 13))
  const button = h(
    'button',
    {
      className: 'copy-id-btn',
      title: t.copyOrderId,
      attrs: { type: 'button', 'aria-label': `${t.copyOrderId}: ${externalId}` },
    },
    iconWrap,
    label,
  )

  let resetTimer: number | null = null
  button.addEventListener('click', () => {
    void navigator.clipboard?.writeText(externalId).then(() => {
      button.classList.add('copied')
      iconWrap.replaceChildren(icon('check', 13))
      label.textContent = t.copiedOrderId
      if (resetTimer !== null) window.clearTimeout(resetTimer)
      resetTimer = window.setTimeout(() => {
        button.classList.remove('copied')
        iconWrap.replaceChildren(icon('copy', 13))
        label.textContent = externalId
      }, 1500)
    }).catch(() => {
      // Clipboard API may be unavailable in restricted contexts.
    })
  })
  return button
}

function renderRowActions(
  record: DonationRecord,
  busy: boolean,
  t: CopyCatalog,
  callbacks: DonationsViewCallbacks,
): HTMLElement {
  const kind = record.visibility.kind
  if (kind === 'private_donor') {
    return h(
      'div',
      { className: 'donation-actions' },
      h('span', { className: 'private-lock' }, icon('lock', 14), t.privateImmutable),
    )
  }

  const actions = h('div', { className: 'donation-actions' })
  if (kind === 'pending_review' || kind === 'below_threshold') {
    actions.appendChild(
      h(
        'button',
        {
          className: 'sm',
          attrs: { type: 'button', disabled: busy },
          on: { click: () => callbacks.onApprove(record) },
        },
        icon(busy ? 'loader' : 'check', 14),
        t.approve,
      ),
    )
  }

  if (kind === 'hidden') {
    actions.appendChild(
      h(
        'button',
        {
          className: 'secondary sm',
          attrs: { type: 'button', disabled: busy },
          on: { click: () => callbacks.onRestore(record) },
        },
        icon(busy ? 'loader' : 'undo', 14),
        t.restore,
      ),
    )
  } else {
    actions.appendChild(
      h(
        'button',
        {
          className: 'quiet danger-text sm',
          attrs: { type: 'button', disabled: busy },
          on: { click: () => callbacks.onRequestHide(record) },
        },
        t.hide,
      ),
    )
  }

  return actions
}

function renderDonationRow(
  record: DonationRecord,
  model: DonationsViewModel,
  callbacks: DonationsViewCallbacks,
): HTMLLIElement {
  const { locale, t, busyIds } = model
  const { visibility } = record
  const busy = busyIds.has(record.id)
  const stamp = formatDateTime(record.donatedAt, locale)

  const reasonTag = visibility.kind === 'pending_review'
    ? h('span', { className: 'reason-tag' }, t.reviewReasons[visibility.reason])
    : null

  const hiddenNote = visibility.kind === 'hidden'
    ? h(
      'p',
      { className: 'moderation-note' },
      t.hiddenNote(visibility.reason, t.statusLabels[visibility.previous.kind]),
    )
    : null

  return h(
    'li',
    { className: visibility.kind === 'pending_review' ? 'donation-row is-pending' : 'donation-row' },
    h(
      'div',
      { className: 'donation-main' },
      h(
        'div',
        { className: 'donation-top' },
        h(
          'span',
          { className: record.displayName === null ? 'donor-name anonymous' : 'donor-name' },
          record.displayName ?? t.anonymousDonor,
        ),
        h('span', { className: 'amount-pill' }, formatDonationMoney(record)),
        h('span', { className: statusBadgeTone(visibility.kind) }, t.statusLabels[visibility.kind]),
        reasonTag,
      ),
      record.message === null ? null : h('p', { className: 'donation-message' }, record.message),
      hiddenNote,
      h(
        'div',
        { className: 'donation-meta' },
        h('span', { className: 'provider-label' }, t.providerLabels[record.provider]),
        h('span', { className: 'meta-sep', attrs: { 'aria-hidden': 'true' } }, '·'),
        renderCopyOrderButton(record.externalId, t),
        h('span', { className: 'meta-sep', attrs: { 'aria-hidden': 'true' } }, '·'),
        h('time', { title: stamp.iso, attrs: { datetime: stamp.iso } }, stamp.label),
      ),
    ),
    renderRowActions(record, busy, t, callbacks),
  )
}

function renderSkeletonList(): HTMLElement {
  return h(
    'div',
    { attrs: { 'aria-busy': 'true' } },
    ...[0, 1, 2, 3].map(() => h(
      'div',
      { className: 'skeleton-row' },
      h('div', { className: 'skeleton-line w-md' }),
      h('div', { className: 'skeleton-line w-lg' }),
      h('div', { className: 'skeleton-line w-sm' }),
    )),
  )
}

function renderEmptyList(model: DonationsViewModel, callbacks: DonationsViewCallbacks): HTMLElement {
  const { counts, visibilityFilter, providerFilter, searchQuery, afdianSyncConfigured, t } = model
  const hasFilter = visibilityFilter !== 'all' || providerFilter !== 'all' || searchQuery.trim().length > 0
  if (hasFilter || counts.total > 0) {
    return h(
      'div',
      { className: 'empty' },
      h('span', { className: 'empty-icon' }, icon('search', 22)),
      h('h2', null, t.emptyFilteredTitle),
      hasFilter
        ? h(
          'button',
          { className: 'secondary', attrs: { type: 'button' }, on: { click: callbacks.onResetFilters } },
          t.resetFilters,
        )
        : null,
    )
  }

  return h(
    'div',
    { className: 'empty' },
    h('span', { className: 'empty-icon' }, icon('heart', 22)),
    h('h2', null, t.emptyTitle),
    h(
      'div',
      { className: 'empty-actions' },
      h(
        'button',
        { attrs: { type: 'button' }, on: { click: callbacks.onOpenManualDialog } },
        icon('plus', 16),
        t.addDonation,
      ),
      afdianSyncConfigured
        ? h(
          'button',
          { className: 'secondary', attrs: { type: 'button' }, on: { click: callbacks.onSyncAfdian } },
          icon('sync', 16),
          t.syncAfdian,
        )
        : null,
    ),
  )
}

function renderFilterTabButtons(
  model: DonationsViewModel,
  onSelect: (filter: VisibilityFilter) => void,
): HTMLButtonElement[] {
  const { counts, visibilityFilter, locale, t } = model
  return FILTER_ORDER.map(filter => {
    const count = filterCountValue(counts, filter)
    const label = filter === 'all' ? t.filterAll : t.filterLabels[filter]
    const isPendingHot = filter === 'pending_review' && count > 0
    return h(
      'button',
      {
        className: isPendingHot ? 'filter-tab has-pending' : 'filter-tab',
        attrs: {
          type: 'button',
          'aria-pressed': String(visibilityFilter === filter),
        },
        on: { click: () => onSelect(filter) },
      },
      label,
      h('span', { className: 'filter-count' }, formatCount(count, locale)),
    )
  })
}

function syncProviderSelect(select: HTMLSelectElement, model: DonationsViewModel): void {
  const { t, providerFilter } = model
  select.setAttribute('aria-label', t.providerAll)
  select.replaceChildren(
    h('option', { attrs: { value: 'all' } }, t.providerAll),
    ...DONATION_PROVIDERS.map(provider => h(
      'option',
      { attrs: { value: provider } },
      t.providerLabels[provider],
    )),
  )
  select.value = providerFilter
}

function renderListFooter(
  model: DonationsViewModel,
  onLoadMore: () => void,
): HTMLElement | null {
  if (model.nextCursor === null) return null
  const { items, loadingMore, t } = model
  return h(
    'div',
    { className: 'list-footer' },
    h('span', null, t.showingCount(items.length)),
    h(
      'button',
      {
        className: 'secondary sm',
        attrs: { type: 'button', disabled: loadingMore },
        on: { click: onLoadMore },
      },
      loadingMore ? icon('loader', 14) : null,
      loadingMore ? t.loadingMore : t.loadMore,
    ),
  )
}

export interface DonationsViewHandle {
  readonly element: HTMLElement
  update(model: DonationsViewModel): void
}

export function createDonationsView(
  initial: DonationsViewModel,
  callbacks: DonationsViewCallbacks,
): DonationsViewHandle {
  const heading = h('h1', null, initial.t.navDonations)
  const pillsSlot = h('div')
  const noticeSlot = h('div')
  const overviewSlot = h('div')
  const filterTabs = h('div', { className: 'filter-tabs' })
  const listSlot = h('div')
  const footerSlot = h('div')

  const providerSelect = h('select', {
    className: 'provider-select',
    attrs: { 'aria-label': initial.t.providerAll },
    on: { change: () => callbacks.onSelectProvider(providerSelect.value as ProviderFilter) },
  })

  const searchInput = h('input', {
    attrs: {
      type: 'search',
      placeholder: initial.t.searchPlaceholder,
      'aria-label': initial.t.searchPlaceholder,
      autocomplete: 'off',
    },
  })
  const searchClearBtn = h(
    'button',
    {
      className: 'quiet search-clear',
      title: initial.t.clearSearch,
      attrs: { type: 'button', 'aria-label': initial.t.clearSearch, hidden: true },
      on: {
        click: () => {
          searchInput.value = ''
          searchClearBtn.hidden = true
          callbacks.onSearchChange('')
          searchInput.focus()
        },
      },
    },
    icon('close', 14),
  )

  searchInput.addEventListener('input', () => {
    searchClearBtn.hidden = searchInput.value.length === 0
    callbacks.onSearchChange(searchInput.value)
  })
  searchInput.addEventListener('keydown', event => {
    if (event.key === 'Escape' && searchInput.value.length > 0) {
      searchInput.value = ''
      searchClearBtn.hidden = true
      callbacks.onSearchChange('')
    }
  })

  const toolbar = h(
    'div',
    { className: 'toolbar' },
    filterTabs,
    h(
      'div',
      { className: 'toolbar-controls' },
      providerSelect,
      h(
        'div',
        { className: 'search-box' },
        h('span', { className: 'search-icon' }, icon('search', 15)),
        searchInput,
        searchClearBtn,
      ),
    ),
  )

  const root = h(
    'div',
    { className: 'view' },
    h('div', { className: 'page-title' }, heading, pillsSlot),
    noticeSlot,
    overviewSlot,
    h('section', { className: 'panel donations-panel' }, toolbar, listSlot, footerSlot),
  )

  const update = (model: DonationsViewModel): void => {
    const { t, items, loading, searchQuery } = model
    heading.textContent = t.navDonations
    const pills = renderStatusPills(model.config, t)
    pillsSlot.replaceChildren(...(pills === null ? [] : [pills]))

    const noticeNode = model.loadError !== null
      ? renderReadError(model.loadError, t, callbacks.onRetryLoad)
      : renderNotice(model.notice, t, callbacks.onDismissNotice)
    noticeSlot.replaceChildren(...(noticeNode === null ? [] : [noticeNode]))
    noticeSlot.hidden = noticeNode === null

    overviewSlot.replaceChildren(renderOverviewStrip(model, callbacks.onSelectVisibility))
    filterTabs.replaceChildren(...renderFilterTabButtons(model, callbacks.onSelectVisibility))
    syncProviderSelect(providerSelect, model)

    searchInput.placeholder = t.searchPlaceholder
    searchInput.setAttribute('aria-label', t.searchPlaceholder)
    searchClearBtn.title = t.clearSearch
    searchClearBtn.setAttribute('aria-label', t.clearSearch)
    if (document.activeElement !== searchInput && searchInput.value !== searchQuery) {
      searchInput.value = searchQuery
    }
    searchClearBtn.hidden = searchInput.value.length === 0

    if (loading && items.length === 0) {
      listSlot.replaceChildren(renderSkeletonList())
      footerSlot.replaceChildren()
      return
    }
    if (items.length === 0) {
      listSlot.replaceChildren(renderEmptyList(model, callbacks))
      footerSlot.replaceChildren()
      return
    }

    listSlot.replaceChildren(
      h('ul', { className: 'donation-list' }, ...items.map(item => renderDonationRow(item, model, callbacks))),
    )
    const footer = renderListFooter(model, callbacks.onLoadMore)
    footerSlot.replaceChildren(...(footer === null ? [] : [footer]))
  }

  update(initial)
  return { element: root, update }
}
