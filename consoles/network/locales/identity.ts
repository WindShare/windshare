import type { Translations } from '../../ui/locale'

const en = {
  node: 'Enter a complete, valid node public key.',
  account: 'Enter a complete, valid account ID.',
  share: 'Enter a complete, valid share ID.',
}

export const identityErrors = {
  en,
  'zh-CN': {
    node: '请输入完整有效的节点公钥。',
    account: '请输入完整有效的账号 ID。',
    share: '请输入完整有效的分享 ID。',
  },
} satisfies Translations<typeof en>
