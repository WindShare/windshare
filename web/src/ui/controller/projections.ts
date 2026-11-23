import type { ReceiverSnapshot } from '../model'
import { TaskComposer, type RetainedAdmission, type ActiveAdmission, type TaskComposition } from '../experience/task-composition'

export class PresentationRegion<T> {
  readonly #listeners = new Set<() => void>()
  #snapshot: T
  #changed = false
  constructor(initial: T) { this.#snapshot = initial }
  readonly getSnapshot = (): T => this.#snapshot
  readonly subscribe = (listener: () => void): (() => void) => {
    this.#listeners.add(listener)
    return () => { this.#listeners.delete(listener) }
  }
  stage(next: T): void {
    if (sameFields(this.#snapshot, next)) return
    this.#snapshot = next
    this.#changed = true
  }
  publish(): void {
    if (!this.#changed) return
    this.#changed = false
    for (const listener of this.#listeners) {
      try { listener() } catch { /* A view cannot prevent another region from receiving the revision. */ }
    }
  }
  clear(): void { this.#listeners.clear() }
}

function sameFields<T>(left: T, right: T): boolean {
  return left === right || (left !== null && right !== null && typeof left === 'object' && typeof right === 'object' &&
    Object.keys(right).every(key => left[key as keyof T] === right[key as keyof T]))
}

function browse(snapshot: ReceiverSnapshot) {
  const { share, rows, breadcrumbs, pageIndex, pageCount, omittedCount, browse, draft, preview, startAdmission } = snapshot
  const { capabilitiesState, activeSingleFileId, singleFilePickerPendingFromBytes, language } = snapshot.output
  return Object.freeze({
    share, rows, breadcrumbs, pageIndex, pageCount, omittedCount, browse, draft, preview,
    startAdmission, capabilitiesState, activeSingleFileId, singleFilePickerPendingFromBytes, language,
  })
}
function connection(snapshot: ReceiverSnapshot) {
  const { phase, status, error, connection, pathActivity } = snapshot
  return Object.freeze({ phase, status, error, connection, pathActivity })
}
function task(snapshot: ReceiverSnapshot) {
  const { output, progress, taskDisplay, connection, share, retained, activeReceiveOperationId, startAdmission, draft } = snapshot
  return Object.freeze({ output, progress, taskDisplay, connection, share, retained, activeReceiveOperationId, startAdmission, draft,
    hasContent: share !== null || snapshot.breadcrumbs.length > 0 })
}
function downloads(snapshot: ReceiverSnapshot, composition: TaskComposition) {
  return Object.freeze({ retained: snapshot.retained, history: composition.history,
    activeId: composition.current?.operationId ?? null, attention: composition.current?.attention ?? false })
}
function shareScope(snapshot: ReceiverSnapshot, visitId: number) {
  return Object.freeze({
    kind: snapshot.share?.kind ?? ('pending' as const),
    shareInstance: snapshot.share?.shareInstance ?? null,
    visitId,
  })
}
export type TaskSnapshot = ReturnType<typeof task>
export type ShareScopeSnapshot = ReturnType<typeof shareScope>

/** All regions are staged before notifying any reader, so cross-region reads share one revision. */
export class ReceiverProjections {
  readonly browsing: PresentationRegion<ReturnType<typeof browse>>
  readonly connection: PresentationRegion<ReturnType<typeof connection>>
  readonly task: PresentationRegion<TaskSnapshot & TaskComposition>
  readonly downloads: PresentationRegion<ReturnType<typeof downloads>>
  readonly surface: PresentationRegion<boolean>
  readonly shareScope: PresentationRegion<ShareScopeSnapshot>
  readonly #composer = new TaskComposer()
  readonly #admission: RetainedAdmission
  readonly #activeAdmission: ActiveAdmission
  #lastShareInstance: string | null
  #shareVisitId = 0

  constructor(snapshot: ReceiverSnapshot, admission: RetainedAdmission, activeAdmission: ActiveAdmission) {
    this.#admission = admission
    this.#activeAdmission = activeAdmission
    this.#lastShareInstance = snapshot.share?.shareInstance ?? null
    this.browsing = new PresentationRegion(browse(snapshot))
    this.connection = new PresentationRegion(connection(snapshot))
    const composition = this.#composer.compose(snapshot, admission, activeAdmission)
    this.task = new PresentationRegion({ ...task(snapshot), ...composition })
    this.downloads = new PresentationRegion(downloads(snapshot, composition))
    this.surface = new PresentationRegion(this.#receiving(snapshot))
    this.shareScope = new PresentationRegion(shareScope(snapshot, this.#shareVisitId))
  }
  commit(snapshot: ReceiverSnapshot): void {
    this.browsing.stage(browse(snapshot))
    this.connection.stage(connection(snapshot))
    const composition = this.#composer.compose(snapshot, this.#admission, this.#activeAdmission)
    this.task.stage({ ...task(snapshot), ...composition })
    this.downloads.stage(downloads(snapshot, composition))
    this.surface.stage(this.#receiving(snapshot))
    const nextShareInstance = snapshot.share?.shareInstance ?? null
    if (nextShareInstance !== this.#lastShareInstance) {
      this.#lastShareInstance = nextShareInstance
      this.#shareVisitId += 1
    }
    this.shareScope.stage(shareScope(snapshot, this.#shareVisitId))
    this.browsing.publish()
    this.connection.publish()
    this.task.publish()
    this.downloads.publish()
    this.surface.publish()
    this.shareScope.publish()
  }
  close(): void {
    this.browsing.clear(); this.connection.clear(); this.task.clear(); this.surface.clear(); this.shareScope.clear(); this.downloads.clear()
  }
  #receiving(snapshot: ReceiverSnapshot): boolean {
    return snapshot.share !== null || snapshot.breadcrumbs.length > 0 ||
      snapshot.phase === 'joining' || snapshot.phase === 'browsing' || (snapshot.phase === 'failed' && snapshot.error !== null)
  }
}
