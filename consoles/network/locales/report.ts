import type { Translations } from '../../ui/locale'

const en = {
  title: 'Report a share',
  network: 'Official relay network',
  received: 'Report received',
  review: 'A network administrator will review your report.',
  publicShareId: 'Public share ID',
  reason: 'Reason for reporting',
  missingReason: 'Enter a reason for your report.',
  longReason: 'Shorten the reason to 2,000 UTF-8 bytes or fewer.',
  rateLimited: 'Too many reports. Please try again later.',
  submitting: 'Submitting…',
  submit: 'Submit report',
}

export const reportCopy = {
  en,
  'zh-CN': {
    title: '举报分享',
    network: '官方中转网络',
    received: '已收到举报',
    review: '网络管理员将审核你的举报。',
    publicShareId: '公开分享 ID',
    reason: '举报原因',
    missingReason: '请填写举报原因。',
    longReason: '举报原因不能超过 2,000 个 UTF-8 字节，请缩短内容。',
    rateLimited: '举报过于频繁，请稍后再试。',
    submitting: '正在提交…',
    submit: '提交举报',
  },
} satisfies Translations<typeof en>
