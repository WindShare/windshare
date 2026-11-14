import { IndexedDbDirectZipJournalRepository } from '../../../output/direct-zip/journal/indexeddb'
import type { DirectZipJournalRepository } from '../../../output/direct-zip/journal/repository'
import { DEFAULT_DIRECT_ZIP_RUNTIME_POLICY_V1 } from '../../../output/direct-zip/session'
import type { BrowserDirectZipCapabilitySource } from './contracts'

export const OWNED_TARGET_AUTHORITY = Object.freeze({
  kind: 'owned-target-session-v1' as const,
  recovery: 'persisted-handle-and-verified-checkpoint' as const,
  replacement: 'coordinated-no-replace' as const,
  cleanup: 'ownership-proof-required' as const,
})

export function browserDirectZipCapabilities(
  openJournal: () => Promise<DirectZipJournalRepository> = () => IndexedDbDirectZipJournalRepository.open(),
): BrowserDirectZipCapabilitySource {
  return {
    read: async signal => {
      signal.throwIfAborted()
      let journal: DirectZipJournalRepository | undefined
      try {
        journal = await openJournal()
        signal.throwIfAborted()
        return { authority: OWNED_TARGET_AUTHORITY, policy: DEFAULT_DIRECT_ZIP_RUNTIME_POLICY_V1 }
      } catch {
        signal.throwIfAborted()
        return { authority: { kind: 'unavailable' as const, reason: 'journal-unavailable' as const } }
      } finally { journal?.close() }
    },
  }
}
