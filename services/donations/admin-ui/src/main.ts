import {
  defaultDonationConfig,
  emptyVisibilityCounts,
  type AdminConfigPatch,
  type AdminDonationsFilter,
  type AdminVisibilityCommand,
  type AdminVisibilityCounts,
  type DonationConfig,
  type DonationRecord,
} from '../../src/model'
import { AdminApiClient, formatApiError, type ManualDonationPayload } from './api'
import {
  renderMasthead,
  renderSessionWait,
  renderWorkspaceBar,
  type FeedbackNotice,
  type LocalizedText,
} from './chrome'
import {
  createDonationsView,
  type DonationsViewCallbacks,
  type DonationsViewHandle,
  type DonationsViewModel,
  type ProviderFilter,
  type VisibilityFilter,
} from './donations-view'
import { h } from './dom'
import { openHideDialog } from './hide-dialog'
import { getCopy } from './locales'
import { renderLoginView } from './login-view'
import { openManualDonationDialog } from './manual-dialog'
import {
  applyAppearance,
  loadInitialAppearance,
  loadInitialLocale,
  saveAppearance,
  saveLocale,
  type Appearance,
  type Locale,
} from './preferences'
import {
  buildConfigPatch,
  configToDraft,
  createSettingsView,
  type SettingsDraft,
  type SettingsViewCallbacks,
  type SettingsViewHandle,
} from './settings-view'

type AuthPhase = 'checking' | 'signed_out' | 'signed_in'
type ActiveTab = 'donations' | 'settings'

const SEARCH_DEBOUNCE_MS = 260

interface AppState {
  locale: Locale
  appearance: Appearance
  phase: AuthPhase
  activeTab: ActiveTab
  sessionError: LocalizedText | null
  loginBusy: boolean
  loginFeedback: FeedbackNotice | null
  signingOut: boolean
  afdianSyncConfigured: boolean
  config: DonationConfig
  settingsDraft: SettingsDraft
  settingsSaving: boolean
  settingsFeedback: FeedbackNotice | null
  counts: AdminVisibilityCounts
  items: DonationRecord[]
  nextCursor: string | null
  visibilityFilter: VisibilityFilter
  providerFilter: ProviderFilter
  searchQuery: string
  listLoading: boolean
  loadingMore: boolean
  syncingAfdian: boolean
  loadError: LocalizedText | null
  donationsNotice: FeedbackNotice | null
  busyIds: Set<string>
}

function parseTabFromHash(): ActiveTab {
  return window.location.hash === '#settings' ? 'settings' : 'donations'
}

function createInitialState(): AppState {
  const initialConfig = defaultDonationConfig()
  return {
    locale: loadInitialLocale(),
    appearance: loadInitialAppearance(),
    phase: 'checking',
    activeTab: parseTabFromHash(),
    sessionError: null,
    loginBusy: false,
    loginFeedback: null,
    signingOut: false,
    afdianSyncConfigured: true,
    config: initialConfig,
    settingsDraft: configToDraft(initialConfig),
    settingsSaving: false,
    settingsFeedback: null,
    counts: emptyVisibilityCounts(),
    items: [],
    nextCursor: null,
    visibilityFilter: 'all',
    providerFilter: 'all',
    searchQuery: '',
    listLoading: false,
    loadingMore: false,
    syncingAfdian: false,
    loadError: null,
    donationsNotice: null,
    busyIds: new Set<string>(),
  }
}

function buildFilterPayload(state: AppState): AdminDonationsFilter | undefined {
  const visibility = state.visibilityFilter === 'all' ? undefined : state.visibilityFilter
  const provider = state.providerFilter === 'all' ? undefined : state.providerFilter
  const trimmedQuery = state.searchQuery.trim()
  const query = trimmedQuery.length > 0 ? trimmedQuery : undefined
  if (visibility === undefined && provider === undefined && query === undefined) {
    return undefined
  }
  return {
    ...(visibility === undefined ? {} : { visibility }),
    ...(provider === undefined ? {} : { provider }),
    ...(query === undefined ? {} : { query }),
  }
}

class DonationsAdminApp {
  private readonly state: AppState = createInitialState()
  private readonly api: AdminApiClient
  private readonly mastheadSlot = h('div')
  private readonly mainEl = h('main', { id: 'main-content', attrs: { tabindex: '-1' } })
  private readonly workspaceBarSlot = h('div')
  private readonly workspaceViewSlot = h('div')
  private readonly skipLink: HTMLAnchorElement

  private listAbort: AbortController | null = null
  private searchTimer: number | null = null
  private donationsView: DonationsViewHandle | null = null
  private settingsView: SettingsViewHandle | null = null
  private activeDialog: HTMLDialogElement | null = null
  private mountedPhase: AuthPhase | null = null
  private mountedTab: ActiveTab | null = null

  constructor(mount: HTMLElement) {
    applyAppearance(this.state.appearance)
    this.api = new AdminApiClient(() => {
      this.transitionToSignedOut({ kind: 'warning', format: t => t.sessionExpired })
    })

    this.skipLink = h('a', {
      className: 'skip',
      attrs: { href: '#main-content' },
      on: {
        click: event => {
          event.preventDefault()
          this.mainEl.focus()
        },
      },
    })
    mount.replaceChildren(this.skipLink, this.mastheadSlot, this.mainEl)

    window.addEventListener('hashchange', () => {
      const nextTab = parseTabFromHash()
      if (this.state.activeTab === nextTab) return
      this.state.activeTab = nextTab
      this.render()
    })
  }

  start(): void {
    void this.probeSession()
  }

  private transitionToSignedOut(feedback: FeedbackNotice | null): void {
    this.listAbort?.abort()
    this.listAbort = null
    this.activeDialog?.close()
    this.activeDialog?.remove()
    this.activeDialog = null
    this.state.phase = 'signed_out'
    this.state.items = []
    this.state.nextCursor = null
    this.state.counts = emptyVisibilityCounts()
    this.state.busyIds.clear()
    this.state.loginBusy = false
    this.state.signingOut = false
    this.state.loginFeedback = feedback
    this.render()
  }

  private applyServerConfig(nextConfig: DonationConfig): void {
    const evaluation = buildConfigPatch(this.state.config, this.state.settingsDraft)
    const wasDirty = !evaluation.valid || evaluation.dirty
    this.state.config = nextConfig
    if (!wasDirty) {
      this.state.settingsDraft = configToDraft(nextConfig)
    }
  }

  private makeDonationsModel(): DonationsViewModel {
    return {
      locale: this.state.locale,
      t: getCopy(this.state.locale),
      items: this.state.items,
      counts: this.state.counts,
      config: this.state.config,
      nextCursor: this.state.nextCursor,
      visibilityFilter: this.state.visibilityFilter,
      providerFilter: this.state.providerFilter,
      searchQuery: this.state.searchQuery,
      loading: this.state.listLoading,
      loadingMore: this.state.loadingMore,
      busyIds: this.state.busyIds,
      afdianSyncConfigured: this.state.afdianSyncConfigured,
      loadError: this.state.loadError,
      notice: this.state.donationsNotice,
    }
  }

  private renderMastheadAndChrome(): void {
    const t = getCopy(this.state.locale)
    document.documentElement.lang = this.state.locale
    document.title = `${t.areaTitle} · WindShare`
    this.skipLink.textContent = t.skip
    this.mastheadSlot.replaceChildren(
      renderMasthead({
        locale: this.state.locale,
        appearance: this.state.appearance,
        authenticated: this.state.phase === 'signed_in',
        signingOut: this.state.signingOut,
        t,
        onSelectLocale: next => {
          this.state.locale = next
          saveLocale(next)
          this.render()
        },
        onSelectAppearance: next => {
          this.state.appearance = next
          saveAppearance(next)
          this.renderMastheadAndChrome()
        },
        onSignOut: () => void this.handleSignOut(),
      }),
    )

    if (this.state.phase === 'signed_in') {
      this.workspaceBarSlot.replaceChildren(
        renderWorkspaceBar({
          activeTab: this.state.activeTab,
          pendingCount: this.state.counts.pending_review,
          settingsAttention:
            this.state.config.moderationEnabled || this.state.config.cryptoCheckoutPaused,
          afdianSyncConfigured: this.state.afdianSyncConfigured,
          syncing: this.state.syncingAfdian,
          refreshing: this.state.listLoading,
          t,
          onOpenManualDialog: () => this.openManualDialog(),
          onSyncAfdian: () => void this.handleSyncAfdian(),
          onRefresh: () => void this.loadDonations(true),
        }),
      )
    }
  }

  private renderSignedInWorkspace(): void {
    if (this.mountedPhase !== 'signed_in') {
      const workspace = h('div', { className: 'workspace' }, this.workspaceBarSlot, this.workspaceViewSlot)
      this.mainEl.replaceChildren(workspace)
      this.mountedPhase = 'signed_in'
      this.mountedTab = null
    }

    if (this.state.activeTab === 'donations') {
      if (this.donationsView === null) {
        this.donationsView = createDonationsView(this.makeDonationsModel(), this.donationsCallbacks())
      } else {
        this.donationsView.update(this.makeDonationsModel())
      }
      if (this.mountedTab !== 'donations') {
        this.workspaceViewSlot.replaceChildren(this.donationsView.element)
        this.mountedTab = 'donations'
      }
      return
    }

    const settingsModel = {
      t: getCopy(this.state.locale),
      savedConfig: this.state.config,
      draft: this.state.settingsDraft,
      saving: this.state.settingsSaving,
      feedback: this.state.settingsFeedback,
    }
    if (this.settingsView === null) {
      this.settingsView = createSettingsView(settingsModel, this.settingsCallbacks())
    } else {
      this.settingsView.update(settingsModel)
    }
    if (this.mountedTab !== 'settings') {
      this.workspaceViewSlot.replaceChildren(this.settingsView.element)
      this.mountedTab = 'settings'
    }
  }

  private render(): void {
    const t = getCopy(this.state.locale)
    this.renderMastheadAndChrome()

    if (this.state.phase === 'checking') {
      this.mountedPhase = 'checking'
      this.mountedTab = null
      this.mainEl.replaceChildren(renderSessionWait(this.state.sessionError, t, () => void this.probeSession()))
      return
    }
    if (this.state.phase === 'signed_out') {
      this.mountedPhase = 'signed_out'
      this.mountedTab = null
      this.mainEl.replaceChildren(
        renderLoginView({
          t,
          busy: this.state.loginBusy,
          feedback: this.state.loginFeedback,
          onSubmit: password => void this.handleLogin(password),
        }),
      )
      return
    }
    this.renderSignedInWorkspace()
  }

  private async loadDonations(reset: boolean): Promise<boolean> {
    if (reset) {
      this.listAbort?.abort()
      this.listAbort = new AbortController()
      this.state.listLoading = true
      this.state.loadError = null
    } else {
      if (this.state.loadingMore || this.state.nextCursor === null) return false
      this.state.loadingMore = true
    }
    this.render()

    const signal = reset ? this.listAbort?.signal : undefined
    const filter = buildFilterPayload(this.state)
    const res = await this.api.listDonations(
      {
        ...(reset ? {} : { after: this.state.nextCursor }),
        ...(filter === undefined ? {} : { filter }),
      },
      signal,
    )

    if (reset) this.state.listLoading = false
    else this.state.loadingMore = false

    if (res === null) {
      this.render()
      return false
    }
    if (!res.ok) {
      this.state.loadError = t => formatApiError(res, t)
      this.render()
      return false
    }

    this.applyServerConfig(res.data.config)
    this.state.counts = res.data.counts
    this.state.items = reset ? [...res.data.items] : [...this.state.items, ...res.data.items]
    this.state.nextCursor = res.data.nextCursor
    this.render()
    return true
  }

  private async handleModeration(
    record: DonationRecord,
    command: AdminVisibilityCommand,
  ): Promise<string | null> {
    if (this.state.busyIds.has(record.id)) return null
    this.state.busyIds.add(record.id)
    this.render()

    const res = await this.api.moderate(record.id, command)
    this.state.busyIds.delete(record.id)
    if (res === null) return null

    if (!res.ok) {
      const errorText = formatApiError(res, getCopy(this.state.locale))
      if (command.action !== 'hide') {
        this.state.donationsNotice = { kind: 'error', format: t => formatApiError(res, t) }
        this.render()
      }
      return errorText
    }

    this.state.counts = res.data.counts
    const updated = res.data.record
    const stillMatches = this.state.visibilityFilter === 'all'
      || this.state.visibilityFilter === updated.visibility.kind
    this.state.items = stillMatches
      ? this.state.items.map(item => (item.id === updated.id ? updated : item))
      : this.state.items.filter(item => item.id !== updated.id)

    this.state.donationsNotice = {
      kind: 'success',
      format: t => {
        if (command.action === 'approve') return t.approvedNotice
        if (command.action === 'hide') return t.hiddenNotice
        return t.restoredNotice
      },
    }
    this.render()
    return null
  }

  private async handleSyncAfdian(): Promise<void> {
    if (this.state.syncingAfdian || !this.state.afdianSyncConfigured) return
    this.state.syncingAfdian = true
    this.state.donationsNotice = null
    this.render()

    const res = await this.api.syncAfdian()
    this.state.syncingAfdian = false
    if (res === null) return

    if (!res.ok) {
      this.state.donationsNotice = { kind: 'error', format: t => formatApiError(res, t) }
      this.render()
      return
    }

    const { committed, deduplicated, deferred } = res.data
    this.state.donationsNotice = {
      kind: deferred > 0 ? 'warning' : 'success',
      format: t => t.syncedNotice(committed, deduplicated, deferred),
    }
    await this.loadDonations(true)
  }

  private async handleCreateManualDonation(
    payload: ManualDonationPayload,
  ): Promise<string | null> {
    const res = await this.api.createManualDonation(payload)
    if (res === null) return null
    if (!res.ok) {
      return formatApiError(res, getCopy(this.state.locale))
    }

    this.state.counts = res.data.counts
    this.state.donationsNotice = { kind: 'success', format: t => t.manualCreatedNotice }
    if (this.state.activeTab !== 'donations') {
      this.state.activeTab = 'donations'
      window.location.hash = '#donations'
    }
    void this.loadDonations(true)
    return null
  }

  private openManualDialog(): void {
    this.activeDialog?.close()
    this.activeDialog?.remove()
    this.activeDialog = openManualDonationDialog({
      t: getCopy(this.state.locale),
      onSubmit: payload => this.handleCreateManualDonation(payload),
      onClose: () => {
        this.activeDialog = null
      },
    })
  }

  private async handleSaveSettings(patch: AdminConfigPatch): Promise<void> {
    if (this.state.settingsSaving) return
    this.state.settingsSaving = true
    this.state.settingsFeedback = null
    this.render()

    const res = await this.api.patchConfig(patch)
    this.state.settingsSaving = false
    if (res === null) return

    if (!res.ok) {
      this.state.settingsFeedback = { kind: 'error', format: t => formatApiError(res, t) }
      this.render()
      return
    }

    this.state.config = res.data.config
    this.state.settingsDraft = configToDraft(res.data.config)
    this.state.settingsFeedback = { kind: 'success', format: t => t.settingsSaved }
    this.render()
  }

  private async handleLogin(password: string): Promise<void> {
    if (password.length === 0) {
      this.state.loginFeedback = { kind: 'error', format: t => t.errors.requiredPassword }
      this.render()
      return
    }
    this.state.loginBusy = true
    this.state.loginFeedback = null
    this.render()

    const res = await this.api.login(password)
    this.state.loginBusy = false
    if (!res.ok) {
      this.state.loginFeedback = { kind: 'error', format: t => formatApiError(res, t) }
      this.render()
      return
    }

    this.api.setCsrf(res.data.csrfToken)
    this.state.afdianSyncConfigured = res.data.afdianSyncConfigured
    this.state.phase = 'signed_in'
    this.state.loginFeedback = null
    this.state.donationsNotice = null
    this.state.settingsFeedback = null
    this.render()
    await this.loadDonations(true)
  }

  private async handleSignOut(): Promise<void> {
    if (this.state.signingOut) return
    this.state.signingOut = true
    this.render()
    await this.api.logout()
    this.api.clearCsrf()
    this.transitionToSignedOut({ kind: 'success', format: t => t.signedOut })
  }

  private async probeSession(): Promise<void> {
    this.state.phase = 'checking'
    this.state.sessionError = null
    this.render()

    const res = await this.api.checkSession()
    if (res.ok) {
      this.api.setCsrf(res.data.csrfToken)
      this.state.afdianSyncConfigured = res.data.afdianSyncConfigured
      this.state.config = res.data.config
      this.state.settingsDraft = configToDraft(res.data.config)
      this.state.phase = 'signed_in'
      this.render()
      await this.loadDonations(true)
      return
    }
    if (res.status === 401) {
      this.transitionToSignedOut(null)
      return
    }
    if (res.error === 'admin_not_configured') {
      this.transitionToSignedOut({ kind: 'error', format: t => formatApiError(res, t) })
      return
    }
    this.state.sessionError = t => formatApiError(res, t)
    this.render()
  }

  private resetListAndReload(): void {
    this.state.items = []
    this.state.nextCursor = null
    void this.loadDonations(true)
  }

  private donationsCallbacks(): DonationsViewCallbacks {
    return {
      onSelectVisibility: (filter: VisibilityFilter) => {
        if (this.state.visibilityFilter === filter) return
        this.state.visibilityFilter = filter
        this.resetListAndReload()
      },
      onSelectProvider: (provider: ProviderFilter) => {
        if (this.state.providerFilter === provider) return
        this.state.providerFilter = provider
        this.resetListAndReload()
      },
      onSearchChange: (query: string) => {
        this.state.searchQuery = query
        if (this.searchTimer !== null) window.clearTimeout(this.searchTimer)
        this.searchTimer = window.setTimeout(() => {
          this.searchTimer = null
          this.resetListAndReload()
        }, SEARCH_DEBOUNCE_MS)
      },
      onResetFilters: () => {
        if (this.searchTimer !== null) {
          window.clearTimeout(this.searchTimer)
          this.searchTimer = null
        }
        this.state.visibilityFilter = 'all'
        this.state.providerFilter = 'all'
        this.state.searchQuery = ''
        this.resetListAndReload()
      },
      onRetryLoad: () => void this.loadDonations(true),
      onLoadMore: () => void this.loadDonations(false),
      onOpenManualDialog: () => this.openManualDialog(),
      onSyncAfdian: () => void this.handleSyncAfdian(),
      onApprove: (record: DonationRecord) => void this.handleModeration(record, { action: 'approve' }),
      onRestore: (record: DonationRecord) => void this.handleModeration(record, { action: 'restore' }),
      onRequestHide: (record: DonationRecord) => {
        this.activeDialog?.close()
        this.activeDialog?.remove()
        this.activeDialog = openHideDialog({
          record,
          t: getCopy(this.state.locale),
          onSubmit: reason => this.handleModeration(record, { action: 'hide', reason }),
          onClose: () => {
            this.activeDialog = null
          },
        })
      },
      onDismissNotice: () => {
        this.state.donationsNotice = null
        this.render()
      },
    }
  }

  private settingsCallbacks(): SettingsViewCallbacks {
    return {
      onDraftChange: (draft: SettingsDraft) => {
        this.state.settingsDraft = draft
        this.state.settingsFeedback = null
        this.render()
      },
      onDiscard: () => {
        this.state.settingsDraft = configToDraft(this.state.config)
        this.state.settingsFeedback = null
        this.render()
      },
      onSave: (patch: AdminConfigPatch) => void this.handleSaveSettings(patch),
      onDismissFeedback: () => {
        this.state.settingsFeedback = null
        this.render()
      },
    }
  }
}

const mount = document.getElementById('root')
if (mount !== null) {
  new DonationsAdminApp(mount).start()
}
