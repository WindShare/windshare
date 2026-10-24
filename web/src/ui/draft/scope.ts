import type { BrowseDirectory, BrowsePage, JoinedBrowserShare } from '../../application/browser-share/gateway'
import type { SelectionDraft, ShareIdentity } from '../model'
import { projectDraft, scopeSelection, shareIdentityFromRoot } from './model'

type DraftScopeSource = Pick<JoinedBrowserShare,
  'descriptor' | 'selection' | 'replaceSelection' | 'childDirectory'>

interface DraftScopeUpdate {
  readonly share: ShareIdentity | null
  readonly draft: SelectionDraft
  readonly transition: 'scope-initialized' | 'scope-changed' | 'page-updated'
}

export class SelectionDraftScope {
  #directory: BrowseDirectory | undefined

  get directory(): BrowseDirectory | undefined { return this.#directory }

  reset(): void { this.#directory = undefined }

  commitPage(
    joined: DraftScopeSource,
    page: BrowsePage,
    mode: SelectionDraft['mode'],
    currentShare: ShareIdentity | null,
  ): DraftScopeUpdate {
    const share = page.directory.idText === joined.descriptor.syntheticRootId
      ? shareIdentityFromRoot(page, joined.descriptor.shareInstanceId)
      : currentShare
    const initializing = this.#directory === undefined
    let directory = page.directory
    if (initializing && share?.kind === 'browser' && share.singleFolder) {
      const entry = page.entries.find(candidate => candidate.idText === share.homeDirectoryId)
      if (entry?.kind === 'directory') directory = joined.childDirectory(page.directory, entry)
    }
    // The authenticated root already identifies the initial target. Waiting for
    // its listing would delay downloads; replacing that scope later would revoke them.
    const scopeChanged = mode === 'scope' && this.#directory?.idText !== directory.idText
    this.#directory = directory
    if (scopeChanged) joined.replaceSelection(scopeSelection(directory))
    let transition: DraftScopeUpdate['transition'] = 'page-updated'
    if (initializing) transition = 'scope-initialized'
    else if (scopeChanged) transition = 'scope-changed'
    return Object.freeze({
      share,
      draft: projectDraft(mode, directory, joined.selection, share),
      transition,
    })
  }
}
