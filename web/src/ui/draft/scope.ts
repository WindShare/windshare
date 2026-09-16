import type { V2BrowseDirectory, V2BrowsePage, V2JoinedBrowserShare } from '../v2-gateway'
import type { V2SelectionDraft, V2ShareIdentity } from '../v2-model'
import { projectDraft, scopeSelection, shareIdentityFromRoot } from './model'

type DraftScopeSource = Pick<V2JoinedBrowserShare,
  'descriptor' | 'selection' | 'replaceSelection' | 'childDirectory'>

interface DraftScopeUpdate {
  readonly share: V2ShareIdentity | null
  readonly draft: V2SelectionDraft
  readonly transition: 'scope-initialized' | 'scope-changed' | 'page-updated'
}

export class SelectionDraftScope {
  #directory: V2BrowseDirectory | undefined

  get directory(): V2BrowseDirectory | undefined { return this.#directory }

  reset(): void { this.#directory = undefined }

  commitPage(
    joined: DraftScopeSource,
    page: V2BrowsePage,
    mode: V2SelectionDraft['mode'],
    currentShare: V2ShareIdentity | null,
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
