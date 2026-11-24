import type { failures as English } from '../en/failures'

export const failures: typeof English = {
  invalidLink: '分享链接或密钥无效。请重新打开原链接，或让发送方重新分享。',
  secureContext: '此浏览器无法解密分享，请使用支持安全连接的浏览器打开原链接。',
  open: '无法打开分享。请重新打开原链接，并确认发送方在线。',
  permissionDenied: '未授予文件夹访问权限。请允许访问所选文件夹后重试。',
  action: '无法完成此下载操作。',
  directory: '无法加载此文件夹。',
  preview: '无法预览此文件。',
  frame: '无法加载所选视频帧。',
  history: '无法加载此浏览器中保存的下载记录。',
  sourceChangedHint: '共享文件已更改，无法继续下载。请让发送方重新分享并打开新链接，也可以丢弃此下载以清理进度。',
  technical: '技术详情',
}
