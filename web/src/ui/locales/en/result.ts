import { plural } from './plural'

export const result = {
  replacementFiles: 'Files requiring a new download',
  replacementHint: 'Select a file below to download it as a separate task, then choose how to save it.',
  busyReplacementHint: 'Ask the sender to close the app using these files, then select a file below to download it separately.',
  openOriginalShareForReplacement: 'Open the original share link to download affected files.',
  staleSources: (count: bigint) => `${count} ${count === 1n ? 'file stopped' : 'files stopped'} because the original source version changed or was removed.`,
  busySources: (count: bigint) => `${count} ${count === 1n ? 'file stopped because it is' : 'files stopped because they are'} in use by another app on the sender.`,
  unavailableSources: (count: bigint) => `${count} ${count === 1n ? 'file' : 'files'} could not be received from the sender.`,
  retainedPartialZip: 'The other received files remain available in the partial ZIP.',
  retainedPausedZip: 'The original ZIP and its received progress are retained. Continue only retries original versions.',
  retainedPartialFolder: 'Files already saved to your folder are kept.',
  retainedPausedFolder: 'Files already saved to your folder and resumable progress are kept. Continue only retries original versions.',
  combineSummary: (cause: string, retained: string) => `${cause} ${retained}`,
  downloadCurrent: (path: string) => `Download current version of ${path}`,
  retryFile: (path: string) => `Download ${path} again`,
  moreSources: (count: bigint) => `${count} more affected ${plural(count, 'file')}. Browse the share to download them separately.`,
}
