import type { ReceiveBlockReason } from '../../../application/receive/contracts'

export const admission = {
  connectFirst: 'Connect to the share first.',
  emptySelection: 'Select items to download.',
  downloading: 'A download is in progress.',
  busy: 'Another download action is in progress.',
  matchingShare: 'Open the original share link to continue this download.',
  shareEnded: 'This share has ended. Open a new share link to receive more content.',
  connectionUnavailable: 'Reconnect using the share link before continuing this download.',
  activeInAnotherTab: 'This download is active in another browser tab.',
  actionUnavailable: 'This download action is no longer available.',
} satisfies Record<ReceiveBlockReason, string>
