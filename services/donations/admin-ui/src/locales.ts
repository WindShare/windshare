import type { DonationProvider, PendingReviewReason, VisibilityKind } from '../../src/model'
import type { Appearance, Locale } from './preferences'

export interface CopyCatalog {
  readonly areaTitle: string
  readonly adminRole: string
  readonly skip: string
  readonly language: string
  readonly appearance: string
  readonly appearanceOptions: Readonly<Record<Appearance, string>>
  readonly signIn: string
  readonly signingIn: string
  readonly signOut: string
  readonly password: string
  readonly showPassword: string
  readonly hidePassword: string
  readonly opening: string
  readonly openFailed: string
  readonly retry: string
  readonly sessionExpired: string
  readonly navLabel: string
  readonly navDonations: string
  readonly navSettings: string
  readonly addDonation: string
  readonly syncAfdian: string
  readonly syncing: string
  readonly syncNotConfigured: string
  readonly refresh: string
  readonly refreshing: string
  readonly preReviewOn: string
  readonly cryptoPaused: string
  readonly metricTotal: string
  readonly metricPending: string
  readonly metricPublished: string
  readonly metricHidden: string
  readonly filterAll: string
  readonly filterLabels: Readonly<Record<VisibilityKind, string>>
  readonly statusLabels: Readonly<Record<VisibilityKind, string>>
  readonly reviewReasons: Readonly<Record<PendingReviewReason, string>>
  readonly providerAll: string
  readonly providerLabels: Readonly<Record<DonationProvider, string>>
  readonly searchPlaceholder: string
  readonly clearSearch: string
  readonly anonymousDonor: string
  readonly approve: string
  readonly hide: string
  readonly restore: string
  readonly privateImmutable: string
  readonly copyOrderId: string
  readonly copiedOrderId: string
  readonly hiddenNote: (reason: string, previous: string) => string
  readonly showingCount: (loaded: number) => string
  readonly loadMore: string
  readonly loadingMore: string
  readonly emptyTitle: string
  readonly emptyFilteredTitle: string
  readonly resetFilters: string
  readonly hideDialogTitle: string
  readonly hideReasonLabel: string
  readonly hideReasonPlaceholder: string
  readonly hidePresets: readonly string[]
  readonly manualDialogTitle: string
  readonly manualAmountLabel: string
  readonly manualAmountPlaceholder: string
  readonly manualCurrencyLabel: string
  readonly manualProviderLabel: string
  readonly manualNameLabel: string
  readonly manualNamePlaceholder: string
  readonly manualMessageLabel: string
  readonly manualMessagePlaceholder: string
  readonly manualExternalIdLabel: string
  readonly manualExternalIdPlaceholder: string
  readonly manualDonatedAtLabel: string
  readonly confirmAddDonation: string
  readonly addingDonation: string
  readonly cancel: string
  readonly confirmHide: string
  readonly hiding: string
  readonly dismiss: string
  readonly settingsTitle: string
  readonly sectionThresholds: string
  readonly minCnyLabel: string
  readonly minUsdLabel: string
  readonly invalidThreshold: string
  readonly sectionControls: string
  readonly moderationLabel: string
  readonly cryptoPauseLabel: string
  readonly switchOn: string
  readonly switchOff: string
  readonly checkoutActive: string
  readonly checkoutPaused: string
  readonly saveSettings: string
  readonly savingSettings: string
  readonly discard: string
  readonly settingsSaved: string
  readonly signedOut: string
  readonly approvedNotice: string
  readonly hiddenNotice: string
  readonly restoredNotice: string
  readonly manualCreatedNotice: string
  readonly syncedNotice: (committed: number, deduplicated: number, deferred: number) => string
  readonly errors: {
    readonly requiredPassword: string
    readonly requiredAmount: string
    readonly invalidAmount: string
    readonly invalidManualDonation: string
    readonly duplicateExternalId: string
    readonly idempotencyKeyConflict: string
    readonly invalidCredentials: string
    readonly rateLimited: (seconds: number | null) => string
    readonly adminNotConfigured: string
    readonly afdianNotConfigured: string
    readonly afdianQueryFailed: string
    readonly afdianRateLimited: string
    readonly serviceUnavailable: string
    readonly network: string
    readonly fallback: (code: string) => string
  }
}

const en: CopyCatalog = {
  areaTitle: 'Donations',
  adminRole: 'Donations administrator',
  skip: 'Skip to content',
  language: 'Language',
  appearance: 'Appearance',
  appearanceOptions: { system: 'System theme', light: 'Light theme', dark: 'Dark theme' },
  signIn: 'Sign in',
  signingIn: 'Signing in…',
  signOut: 'Sign out',
  password: 'Admin password',
  showPassword: 'Show password',
  hidePassword: 'Hide password',
  opening: 'Opening console…',
  openFailed: 'Could not open console',
  retry: 'Retry',
  sessionExpired: 'Session expired. Sign in again to continue.',
  navLabel: 'Donations management',
  navDonations: 'Donations',
  navSettings: 'Settings',
  addDonation: 'Add donation',
  syncAfdian: 'Sync Afdian',
  syncing: 'Syncing…',
  syncNotConfigured: 'AFDIAN_TOKEN is not configured',
  refresh: 'Refresh',
  refreshing: 'Refreshing…',
  preReviewOn: 'Pre-review on',
  cryptoPaused: 'Crypto paused',
  metricTotal: 'Total',
  metricPending: 'Pending review',
  metricPublished: 'Published',
  metricHidden: 'Hidden',
  filterAll: 'All',
  filterLabels: {
    pending_review: 'Pending',
    published: 'Published',
    below_threshold: 'Below min',
    hidden: 'Hidden',
    private_donor: 'Private',
  },
  statusLabels: {
    pending_review: 'Pending review',
    published: 'Published',
    below_threshold: 'Below threshold',
    hidden: 'Hidden',
    private_donor: 'Private',
  },
  reviewReasons: {
    moderation_mode: 'Pre-review',
    url_detected: 'URL detected',
    pii_detected: 'Contact info',
    unsupported_currency: 'Other currency',
  },
  providerAll: 'All channels',
  providerLabels: {
    afdian: 'Afdian',
    kofi: 'Ko-fi',
    nowpayments: 'Crypto',
    direct: 'Direct',
  },
  searchPlaceholder: 'Search donor, message, ID…',
  clearSearch: 'Clear search',
  anonymousDonor: 'Anonymous',
  approve: 'Approve',
  hide: 'Hide',
  restore: 'Restore',
  privateImmutable: 'Private',
  copyOrderId: 'Copy order ID',
  copiedOrderId: 'Copied',
  hiddenNote: (reason, previous) => `Reason: ${reason} · Restores to ${previous}`,
  showingCount: loaded => `Showing ${loaded}`,
  loadMore: 'Load more',
  loadingMore: 'Loading…',
  emptyTitle: 'No donations yet',
  emptyFilteredTitle: 'No matching donations',
  resetFilters: 'Reset filters',
  hideDialogTitle: 'Hide donation',
  hideReasonLabel: 'Reason',
  hideReasonPlaceholder: 'Optional — defaults to "moderated"',
  hidePresets: [
    'Spam / promotion',
    'External link',
    'Personal contact info',
    'Inappropriate content',
    'Test donation',
  ],
  manualDialogTitle: 'Record donation',
  manualAmountLabel: 'Amount',
  manualAmountPlaceholder: 'e.g. 50 or 88.88',
  manualCurrencyLabel: 'Currency',
  manualProviderLabel: 'Channel',
  manualNameLabel: 'Display name (optional)',
  manualNamePlaceholder: 'Anonymous if left blank',
  manualMessageLabel: 'Public note (optional)',
  manualMessagePlaceholder: 'Shown on the public supporter wall',
  manualExternalIdLabel: 'Reference / Tx ID (optional)',
  manualExternalIdPlaceholder: 'Auto-generated if left blank',
  manualDonatedAtLabel: 'Date & time (optional)',
  confirmAddDonation: 'Publish to wall',
  addingDonation: 'Publishing…',
  cancel: 'Cancel',
  confirmHide: 'Hide from wall',
  hiding: 'Hiding…',
  dismiss: 'Dismiss',
  settingsTitle: 'Wall settings',
  sectionThresholds: 'Public wall thresholds',
  minCnyLabel: 'Minimum CNY',
  minUsdLabel: 'Minimum USD',
  invalidThreshold: 'Enter a non-negative amount with up to 2 decimals.',
  sectionControls: 'Moderation & checkout',
  moderationLabel: 'Review before publish',
  cryptoPauseLabel: 'Pause Crypto checkout',
  switchOn: 'On',
  switchOff: 'Off',
  checkoutActive: 'Active',
  checkoutPaused: 'Paused',
  saveSettings: 'Save settings',
  savingSettings: 'Saving…',
  discard: 'Discard',
  settingsSaved: 'Settings saved.',
  signedOut: 'Signed out.',
  approvedNotice: 'Approved and published to the public wall.',
  hiddenNotice: 'Hidden from the public wall.',
  restoredNotice: 'Restored to previous status.',
  manualCreatedNotice: 'Donation recorded and published to the public wall.',
  syncedNotice: (committed, deduplicated, deferred) => {
    const base = `Synced Afdian: +${committed} new, ${deduplicated} existing`
    return deferred > 0 ? `${base} · ${deferred} remaining — sync again` : `${base}.`
  },
  errors: {
    requiredPassword: 'Enter the admin password.',
    requiredAmount: 'Enter a positive donation amount.',
    invalidAmount: 'Enter a positive amount matching this currency precision.',
    invalidManualDonation: 'Check the entered fields and length limits.',
    duplicateExternalId: 'A donation with this reference ID already exists for this channel.',
    idempotencyKeyConflict: 'This submission was already recorded with earlier details. Close the dialog to view it.',
    invalidCredentials: 'Incorrect password.',
    rateLimited: sec => (sec ? `Too many attempts. Try again in ${sec}s.` : 'Too many attempts. Try again later.'),
    adminNotConfigured: 'Admin password is not configured on this worker.',
    afdianNotConfigured: 'Afdian sync token is not configured.',
    afdianQueryFailed: 'Could not reach Afdian. Please try again.',
    afdianRateLimited: 'Afdian rate limit reached. Try again shortly.',
    serviceUnavailable: 'Service temporarily unavailable. Please try again.',
    network: 'Could not reach the service. Check your connection.',
    fallback: code => `Request failed (${code}).`,
  },
}

const zhCN: CopyCatalog = {
  areaTitle: '捐赠管理台',
  adminRole: '捐赠管理员',
  skip: '跳到主要内容',
  language: '语言',
  appearance: '外观',
  appearanceOptions: { system: '跟随系统', light: '亮色主题', dark: '暗色主题' },
  signIn: '登录',
  signingIn: '正在登录…',
  signOut: '退出登录',
  password: '管理密码',
  showPassword: '显示密码',
  hidePassword: '隐藏密码',
  opening: '正在打开管理台…',
  openFailed: '无法打开管理台',
  retry: '重试',
  sessionExpired: '登录会话已过期，请重新登录。',
  navLabel: '捐赠管理',
  navDonations: '捐赠记录',
  navSettings: '公开墙设置',
  addDonation: '录入赞助',
  syncAfdian: '同步爱发电',
  syncing: '正在同步…',
  syncNotConfigured: '未配置 AFDIAN_TOKEN',
  refresh: '刷新',
  refreshing: '正在刷新…',
  preReviewOn: '先审后显已开启',
  cryptoPaused: '加密收款已暂停',
  metricTotal: '全部记录',
  metricPending: '待审核',
  metricPublished: '已上墙',
  metricHidden: '已隐藏',
  filterAll: '全部',
  filterLabels: {
    pending_review: '待审核',
    published: '已上墙',
    below_threshold: '低于门槛',
    hidden: '已隐藏',
    private_donor: '私密',
  },
  statusLabels: {
    pending_review: '待审核',
    published: '已上墙',
    below_threshold: '低于门槛',
    hidden: '已隐藏',
    private_donor: '私密捐赠',
  },
  reviewReasons: {
    moderation_mode: '先审后显',
    url_detected: '含外链',
    pii_detected: '含联系方式',
    unsupported_currency: '其他币种',
  },
  providerAll: '全部渠道',
  providerLabels: {
    afdian: '爱发电',
    kofi: 'Ko-fi',
    nowpayments: '加密货币',
    direct: '直接赞助',
  },
  searchPlaceholder: '搜索昵称、留言或订单号…',
  clearSearch: '清除搜索',
  anonymousDonor: '匿名支持者',
  approve: '批准上墙',
  hide: '隐藏',
  restore: '恢复',
  privateImmutable: '私密不可公开',
  copyOrderId: '复制订单号',
  copiedOrderId: '已复制',
  hiddenNote: (reason, previous) => `原因：${reason} · 恢复后回到「${previous}」`,
  showingCount: loaded => `已显示 ${loaded} 条`,
  loadMore: '加载更多',
  loadingMore: '正在加载…',
  emptyTitle: '暂无捐赠记录',
  emptyFilteredTitle: '没有匹配的记录',
  resetFilters: '清除筛选',
  hideDialogTitle: '隐藏捐赠记录',
  hideReasonLabel: '隐藏原因',
  hideReasonPlaceholder: '可选，留空默认记为 moderated',
  hidePresets: [
    '垃圾广告 / 推广',
    '包含外链',
    '含个人联系方式',
    '不当内容',
    '测试订单',
  ],
  manualDialogTitle: '录入赞助',
  manualAmountLabel: '金额',
  manualAmountPlaceholder: '例如 50 或 88.88',
  manualCurrencyLabel: '币种',
  manualProviderLabel: '渠道',
  manualNameLabel: '展示昵称（可选）',
  manualNamePlaceholder: '留空记为匿名支持者',
  manualMessageLabel: '公开留言（可选）',
  manualMessagePlaceholder: '将公开展示于致谢墙',
  manualExternalIdLabel: '交易参考号（可选）',
  manualExternalIdPlaceholder: '留空自动生成',
  manualDonatedAtLabel: '赞助时间（可选）',
  confirmAddDonation: '录入并上墙',
  addingDonation: '正在录入…',
  cancel: '取消',
  confirmHide: '确认隐藏',
  hiding: '正在隐藏…',
  dismiss: '关闭提示',
  settingsTitle: '公开墙设置',
  sectionThresholds: '公开上墙门槛',
  minCnyLabel: '人民币最低金额',
  minUsdLabel: '美元最低金额',
  invalidThreshold: '请输入非负金额，最多保留 2 位小数。',
  sectionControls: '审核与收款控制',
  moderationLabel: '先审后显',
  cryptoPauseLabel: '暂停加密货币收款',
  switchOn: '已开启',
  switchOff: '已关闭',
  checkoutActive: '运行中',
  checkoutPaused: '已暂停',
  saveSettings: '保存设置',
  savingSettings: '正在保存…',
  discard: '放弃更改',
  settingsSaved: '设置已保存。',
  signedOut: '已退出登录。',
  approvedNotice: '已批准并展示到公开致谢墙。',
  hiddenNotice: '已从公开致谢墙隐藏。',
  restoredNotice: '已恢复至隐藏前状态。',
  manualCreatedNotice: '已录入并展示到公开致谢墙。',
  syncedNotice: (committed, deduplicated, deferred) => {
    const base = `爱发电同步完成：新增 ${committed} 条，已存在 ${deduplicated} 条`
    return deferred > 0 ? `${base} · 剩余 ${deferred} 条可再次点击同步` : `${base}。`
  },
  errors: {
    requiredPassword: '请输入管理密码。',
    requiredAmount: '请输入有效的赞助金额。',
    invalidAmount: '请输入符合该币种精度的正数金额。',
    invalidManualDonation: '请检查填写内容与长度限制后重试。',
    duplicateExternalId: '该渠道下已存在相同参考号的记录。',
    idempotencyKeyConflict: '此次提交已按先前内容入库，请关闭弹窗核对。',
    invalidCredentials: '管理密码不正确。',
    rateLimited: sec => (sec ? `尝试过于频繁，请 ${sec} 秒后再试。` : '尝试过于频繁，请稍后再试。'),
    adminNotConfigured: '服务未配置有效的管理密码。',
    afdianNotConfigured: '服务未配置爱发电同步密钥。',
    afdianQueryFailed: '无法连接爱发电接口，请稍后重试。',
    afdianRateLimited: '爱发电接口触发限流，请稍后再试。',
    serviceUnavailable: '服务暂时不可用，请稍后重试。',
    network: '无法连接到服务，请检查网络后重试。',
    fallback: code => `请求失败（${code}）。`,
  },
}

const CATALOGS: Readonly<Record<Locale, CopyCatalog>> = {
  en,
  'zh-CN': zhCN,
}

export function getCopy(locale: Locale): CopyCatalog {
  return CATALOGS[locale]
}
