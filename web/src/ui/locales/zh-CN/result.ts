import type { result as English } from '../en/result'

export const result: typeof English = {
  replacementFiles: '需要重新下载的文件',
  replacementHint: '可将当前版本作为独立任务下载。请先在下方选择文件，再选择下载方式。',
  staleSources: count => `有 ${count} 个文件因原版本不可用而停止接收。原 ZIP 及已收到的其他文件会保留；继续原任务只会重试原版本。`,
  downloadCurrent: path => `下载 ${path} 的当前版本`,
  moreSources: count => `另有 ${count} 个受影响的文件。可在分享中找到它们并分别下载当前版本。`,
}
