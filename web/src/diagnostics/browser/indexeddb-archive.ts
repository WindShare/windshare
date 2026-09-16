import {
  isSavedDiagnosticCapture,
  DIAGNOSTICS_ARCHIVE_MAX_AGE_MS,
  isDiagnosticCaptureSummary,
  retainDiagnosticCaptures,
  summarizeDiagnosticCapture,
  type DiagnosticCaptureSummary,
  type DiagnosticsArchiveStore,
  type SavedDiagnosticCapture,
} from './archive'
import type { DiagnosticFile } from './file'

export const DIAGNOSTICS_DATABASE_NAME = 'windshare-diagnostics'
const DATABASE_VERSION = 2
const CAPTURES_STORE = 'captures'
const SUMMARIES_STORE = 'summaries'

export class IndexedDBDiagnosticsArchive implements DiagnosticsArchiveStore {
  readonly #factory: () => IDBFactory
  readonly #now: () => number

  constructor(factory: () => IDBFactory, now: () => number = Date.now) {
    this.#factory = factory
    this.#now = now
  }

  list(): Promise<readonly DiagnosticCaptureSummary[]> {
    return this.#update()
  }

  async readFile(id: string): Promise<DiagnosticFile | null> {
    const database = await this.#open()
    try {
      return await new Promise<DiagnosticFile | null>((resolve, reject) => {
        const transaction = database.transaction(CAPTURES_STORE, 'readonly')
        const request = transaction.objectStore(CAPTURES_STORE).get(id)
        transaction.oncomplete = () => {
          const capture: unknown = request.result
          const now = this.#now()
          const fresh = isSavedDiagnosticCapture(capture) && capture.id === id &&
            capture.savedAt <= now && now - capture.savedAt < DIAGNOSTICS_ARCHIVE_MAX_AGE_MS
          resolve(fresh ? capture.file : null)
        }
        transaction.onabort = transaction.onerror = () => reject(transaction.error ?? new Error('Diagnostic archive read failed'))
      })
    } finally {
      database.close()
    }
  }

  save(capture: SavedDiagnosticCapture): Promise<readonly DiagnosticCaptureSummary[]> {
    return this.#update(capture, summarizeDiagnosticCapture(capture))
  }

  remove(id: string): Promise<readonly DiagnosticCaptureSummary[]> {
    return this.#update(undefined, undefined, id)
  }

  async #update(
    incoming?: SavedDiagnosticCapture,
    summary?: DiagnosticCaptureSummary,
    removedId?: string,
  ): Promise<readonly DiagnosticCaptureSummary[]> {
    const database = await this.#open()
    try {
      return await new Promise<readonly DiagnosticCaptureSummary[]>((resolve, reject) => {
        const transaction = database.transaction([SUMMARIES_STORE, CAPTURES_STORE], 'readwrite')
        const summaries = transaction.objectStore(SUMMARIES_STORE)
        const captures = transaction.objectStore(CAPTURES_STORE)
        const request = summaries.getAll()
        let retained: readonly DiagnosticCaptureSummary[] = []
        transaction.oncomplete = () => resolve(retained)
        transaction.onabort = () => reject(transaction.error ?? new Error('Diagnostic archive transaction aborted'))
        transaction.onerror = () => reject(transaction.error ?? new Error('Diagnostic archive transaction failed'))
        request.onsuccess = () => {
          const stored = request.result as unknown[]
          const candidates = stored.filter(isDiagnosticCaptureSummary)
            .filter(capture => capture.id !== incoming?.id && capture.id !== removedId)
          retained = retainDiagnosticCaptures(summary === undefined ? candidates : [summary, ...candidates], this.#now())
          const retainedIds = new Set(retained.map(capture => capture.id))
          const keys = summaries.getAllKeys()
          keys.onsuccess = () => {
            for (const key of keys.result) {
              if (typeof key !== 'string' || !retainedIds.has(key)) {
                summaries.delete(key)
                captures.delete(key)
              }
            }
            if (incoming !== undefined && retainedIds.has(incoming.id)) {
              summaries.put(summary)
              captures.put(incoming)
            }
          }
        }
      })
    } finally {
      database.close()
    }
  }

  #open(): Promise<IDBDatabase> {
    return new Promise((resolve, reject) => {
      const request = this.#factory().open(DIAGNOSTICS_DATABASE_NAME, DATABASE_VERSION)
      let blocked = false
      request.onupgradeneeded = () => {
        const database = request.result
        // This unreleased cache has no legacy schema to preserve.
        for (const name of [...database.objectStoreNames]) database.deleteObjectStore(name)
        database.createObjectStore(SUMMARIES_STORE, { keyPath: 'id' })
        database.createObjectStore(CAPTURES_STORE, { keyPath: 'id' })
      }
      request.onsuccess = () => {
        if (blocked) request.result.close()
        else {
          request.result.onversionchange = () => request.result.close()
          resolve(request.result)
        }
      }
      request.onerror = () => reject(request.error ?? new Error('Diagnostic archive unavailable'))
      request.onblocked = () => {
        blocked = true
        reject(new Error('Diagnostic archive upgrade is blocked'))
      }
    })
  }
}
