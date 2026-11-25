import type { result as English } from '../en/result'

export const result: typeof English = {
  replacementFiles: '需要重新下载的文件',
  replacementHint: '可在下方选择文件作为独立任务下载，选中后再选择下载方式。',
  busyReplacementHint: '请让发送方关闭占用文件的程序，然后在下方选择文件单独重新下载。',
  staleSources: count => `有 ${count} 个文件因源文件已更改或被移除而停止接收。`,
  busySources: count => `有 ${count} 个文件因正被发送方的其他程序占用而停止接收。`,
  unavailableSources: count => `有 ${count} 个文件无法从发送方接收。`,
  retainedPartialZip: '已收到的其他文件仍保留在部分 ZIP 中，可直接保存。',
  retainedPausedZip: '原 ZIP 及已接收的进度会保留；继续原任务只会重试原版本。',
  retainedPartialFolder: '已保存到文件夹的其他文件会保留。',
  retainedPausedFolder: '已保存到文件夹的文件和可续传进度会保留；继续原任务只会重试原版本。',
  combineSummary: (cause, retained) => `${cause}${retained}`,
  downloadCurrent: path => `下载 ${path} 的当前版本`,
  retryFile: path => `重新下载 ${path}`,
  moreSources: count => `另有 ${count} 个受影响的文件。可在分享中找到它们并分别下载。`,
}
