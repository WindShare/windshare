import type { ReceiverIconName } from '../receiver-presentation/ReceiverIcon'

const IMAGE_EXTENSIONS = new Set(['avif', 'bmp', 'gif', 'heic', 'heif', 'ico', 'jpeg', 'jpg', 'png', 'svg', 'tif', 'tiff', 'webp'])
const VIDEO_EXTENSIONS = new Set(['avi', 'm4v', 'mkv', 'mov', 'mp4', 'mpeg', 'mpg', 'webm'])
const ARCHIVE_EXTENSIONS = new Set(['7z', 'bz2', 'dmg', 'gz', 'iso', 'rar', 'tar', 'tgz', 'xz', 'zip', 'zst'])
const AUDIO_EXTENSIONS = new Set(['aac', 'flac', 'm4a', 'mp3', 'ogg', 'opus', 'wav'])
const DOCUMENT_EXTENSIONS = new Set(['csv', 'doc', 'docx', 'epub', 'md', 'ods', 'odt', 'pdf', 'ppt', 'pptx', 'rtf', 'txt', 'xls', 'xlsx'])
const CODE_EXTENSIONS = new Set(['c', 'cc', 'cpp', 'css', 'go', 'h', 'html', 'java', 'js', 'json', 'jsx', 'mjs', 'py', 'rs', 'sh', 'sql', 'toml', 'ts', 'tsx', 'xml', 'yaml', 'yml'])

const MAX_EXTENSION_BADGE_LENGTH = 5

export interface FileDisplayInfo {
  readonly icon: ReceiverIconName
  readonly typeLabel: string
  readonly badge: string | null
}

function fileExtension(name: string): string | null {
  const dot = name.lastIndexOf('.')
  if (dot <= 0 || dot === name.length - 1) return null
  const ext = name.slice(dot + 1).toLowerCase()
  return /^[a-z0-9]{1,8}$/u.test(ext) ? ext : null
}

/**
 * Purely decorative display metadata for icons and human-readable file kinds.
 * Never grants preview support or asserts decoded media format.
 */
export function fileDisplayInfo(name: string): FileDisplayInfo {
  const ext = fileExtension(name)
  const badge = ext !== null && ext.length <= MAX_EXTENSION_BADGE_LENGTH ? ext.toUpperCase() : null
  if (ext === null) return { icon: 'file', typeLabel: 'File', badge: null }
  if (IMAGE_EXTENSIONS.has(ext)) return { icon: 'image', typeLabel: `${ext.toUpperCase()} image`, badge }
  if (VIDEO_EXTENSIONS.has(ext)) return { icon: 'video', typeLabel: `${ext.toUpperCase()} video`, badge }
  if (ARCHIVE_EXTENSIONS.has(ext)) return { icon: 'archive', typeLabel: `${ext.toUpperCase()} archive`, badge }
  if (AUDIO_EXTENSIONS.has(ext)) return { icon: 'audio', typeLabel: `${ext.toUpperCase()} audio`, badge }
  if (DOCUMENT_EXTENSIONS.has(ext)) return { icon: 'document', typeLabel: ext === 'pdf' ? 'PDF document' : `${ext.toUpperCase()} file`, badge }
  if (CODE_EXTENSIONS.has(ext)) return { icon: 'code', typeLabel: `${ext.toUpperCase()} file`, badge }
  return { icon: 'file', typeLabel: `${ext.toUpperCase()} file`, badge }
}
