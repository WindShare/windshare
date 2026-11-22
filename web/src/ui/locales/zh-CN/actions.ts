import type { actions as English } from '../en/actions'

export const actions: typeof English = {
  pause: '暂停',
  pauseHint: '会先完成当前写入并保存进度，稍后可继续下载。',
  stop: '停止下载',
  stopFolder: '停止下载并清除续传进度。已保存到文件夹的完整文件会保留。',
  stopWorkspace: '停止下载，并整理已保存在浏览器中的文件。',
  stopPortable: '停止下载。直接浏览器下载中断后无法续传。',
  resume: '继续',
  save: '保存',
  saveAgain: '重新保存',
  saveAgainHint: '直接保存浏览器中已接收的副本，无需重新连接发送方。',
  savePartial: '保存部分 ZIP',
  savePartialHint: '将已完成的文件单独保存为一个 ZIP。未完成的项目不会包含在内，原下载稍后仍可继续。',
  discardOutput: '丢弃未完成的下载',
  discardOutputHint: '删除此下载尚未完成的数据和进度。',
  deleteSavedCopy: '删除浏览器中的副本',
  deleteSavedCopyHint: '删除保存在此浏览器中的副本，不会影响已保存到设备的文件。',
  forget: '从下载记录中移除',
  forgetHint: '从下载记录中移除此项。已保存或已交给浏览器的文件不受影响。',
}
