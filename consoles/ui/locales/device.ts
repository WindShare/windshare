import { message } from '../locale'

export const deviceCopy = {
  en: {
    title: 'Authorize this device', match: 'Match the code in WindShare', approve: 'Authorize device',
    approveAs: 'Continue as', switch: 'Use another account', working: 'Authorizing…',
    expired: 'This request has expired or was cancelled. Start again in WindShare.',
    denied: 'Check your username and password.', blocked: 'This account cannot use the service.',
    session: 'The signed-in account has changed. Choose another account to sign in again.',
    limit: 'Too many sign-in attempts. Try again later.', disabled: 'This relay is unavailable.',
    done: 'Device authorized', finish: 'Return to WindShare to continue.',
  },
  'zh-CN': {
    title: '授权此设备', match: '与 WindShare 核对配对码', approve: '授权设备', approveAs: '继续使用',
    switch: '使用其他账号', working: '正在授权…', expired: '请求已过期或已取消，请回到 WindShare 重新连接。',
    denied: '请检查用户名和密码。', blocked: '此账号无法使用该服务。', limit: '登录尝试过于频繁，请稍后再试。',
    session: '登录状态已变更，请选择“使用其他账号”重新登录。',
    disabled: '此中转当前不可用。', done: '设备已授权', finish: '请返回 WindShare 继续。',
  },
}
export const deviceErrors = {
  invalid_invitation: message(deviceCopy, 'expired'), expired_token: message(deviceCopy, 'expired'),
  access_denied: message(deviceCopy, 'expired'), node_unavailable: message(deviceCopy, 'disabled'),
  session_changed: message(deviceCopy, 'session'), credentials: message(deviceCopy, 'denied'), account_blocked: message(deviceCopy, 'blocked'),
  rate_limited: message(deviceCopy, 'limit'),
}
