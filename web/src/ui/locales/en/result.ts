import { plural } from './plural'

export const result = {
  replacementFiles: 'Files requiring a new download',
  replacementHint: 'Download a current version as a separate task. Choose its download option after selecting it below.',
  staleSources: (count: bigint) => `${count} ${count === 1n ? 'file stopped' : 'files stopped'} because the original source version could not be used. The original ZIP and its other received files are retained. Continue retries only the original versions.`,
  downloadCurrent: (path: string) => `Download current version of ${path}`,
  moreSources: (count: bigint) => `${count} more affected ${plural(count, 'file')}. Browse the share to download their current versions separately.`,
}
