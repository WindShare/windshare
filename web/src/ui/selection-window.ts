import type { SelectionRow } from './model'
import { EntrySelectionModel, SELECTION_PAGE_ROWS } from './model'

export interface SelectionWindowSnapshot {
  readonly entries: readonly SelectionRow[]
  readonly manifestEntryCount: number
  readonly selectionPageIndex: number
  readonly selectionPageCount: number
}

/** Keeps React row ownership bounded without hiding any manifest entry. */
export class SelectionPageWindow {
  #pageIndex = 0

  reset(): void {
    this.#pageIndex = 0
  }

  moveTo(
    model: EntrySelectionModel,
    selection: readonly boolean[],
    pageIndex: number,
  ): SelectionWindowSnapshot | undefined {
    const pageCount = this.#pageCount(model)
    if (
      !Number.isSafeInteger(pageIndex) ||
      pageIndex < 0 ||
      pageIndex >= pageCount ||
      pageIndex === this.#pageIndex
    ) {
      return undefined
    }
    this.#pageIndex = pageIndex
    return this.snapshot(model, selection)
  }

  snapshot(
    model: EntrySelectionModel | undefined,
    selection: readonly boolean[],
  ): SelectionWindowSnapshot {
    const manifestEntryCount = model?.entryCount ?? 0
    const selectionPageCount = model === undefined ? 0 : this.#pageCount(model)
    this.#pageIndex = selectionPageCount === 0
      ? 0
      : Math.min(this.#pageIndex, selectionPageCount - 1)
    return Object.freeze({
      entries: model?.rowsWindow(
        selection,
        this.#pageIndex * SELECTION_PAGE_ROWS,
        SELECTION_PAGE_ROWS,
      ) ?? Object.freeze([]),
      manifestEntryCount,
      selectionPageIndex: this.#pageIndex,
      selectionPageCount,
    })
  }

  #pageCount(model: EntrySelectionModel): number {
    return Math.ceil(model.entryCount / SELECTION_PAGE_ROWS)
  }
}
