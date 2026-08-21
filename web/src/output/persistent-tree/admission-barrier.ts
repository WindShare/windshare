export interface MutationAdmission {
  leave(): void
  transferTo(close: () => Promise<void>): void
}

type AdmissionRecord = {
  active: boolean
  closeRequested: boolean
  close?: () => Promise<void>
}

const ADMISSION_CLOSED_MESSAGE = 'Persistent output mutation admission is closed'
const ADMISSION_TRANSFER_MESSAGE = 'Mutation admission has already been transferred'

/**
 * Tracks mutation ownership without putting independent workers onto a shared promise tail.
 * A transferred admission gives shutdown a way to close its long-lived transaction even when
 * the transaction is returned after the shutdown cut was taken.
 */
export class MutationAdmissionBarrier {
  readonly #admissions = new Set<AdmissionRecord>()
  readonly #closeFailures: unknown[] = []
  #accepting = true
  #pendingCloseRequests = 0
  #closePromise: Promise<void> | undefined
  #resolveClose: (() => void) | undefined
  #rejectClose: ((error: unknown) => void) | undefined

  enter(): MutationAdmission {
    if (!this.#accepting) {
      throw new DOMException(ADMISSION_CLOSED_MESSAGE, 'InvalidStateError')
    }
    const record: AdmissionRecord = {
      active: true,
      closeRequested: false,
    }
    this.#admissions.add(record)
    return Object.freeze({
      leave: () => this.#leave(record),
      transferTo: (close: () => Promise<void>) => this.#transfer(record, close),
    })
  }

  closeExternalAdmission(): Promise<void> {
    if (this.#closePromise !== undefined) return this.#closePromise

    this.#accepting = false
    this.#closePromise = new Promise<void>((resolve, reject) => {
      this.#resolveClose = resolve
      this.#rejectClose = reject
    })
    for (const admission of this.#admissions) this.#requestClose(admission)
    this.#finishCloseIfDrained()
    return this.#closePromise
  }

  #transfer(record: AdmissionRecord, close: () => Promise<void>): void {
    if (!record.active) {
      throw new DOMException('Mutation admission has already left', 'InvalidStateError')
    }
    if (record.close !== undefined) throw new TypeError(ADMISSION_TRANSFER_MESSAGE)
    record.close = close
    if (!this.#accepting) this.#requestClose(record)
  }

  #requestClose(record: AdmissionRecord): void {
    if (!record.active || record.closeRequested || record.close === undefined) return
    record.closeRequested = true
    this.#pendingCloseRequests += 1

    let close: Promise<void>
    try {
      close = record.close()
    } catch (error) {
      this.#closeFailures.push(error)
      this.#completeCloseRequest(record)
      return
    }
    Promise.resolve(close).then(
      () => this.#completeCloseRequest(record),
      (error: unknown) => {
        this.#closeFailures.push(error)
        this.#completeCloseRequest(record)
      },
    )
  }

  #completeCloseRequest(record: AdmissionRecord): void {
    // A well-behaved transaction leaves from its own close hook. Releasing here as well
    // keeps a failed close implementation from making the shutdown drain wait forever.
    this.#leave(record)
    this.#pendingCloseRequests -= 1
    this.#finishCloseIfDrained()
  }

  #leave(record: AdmissionRecord): void {
    if (!record.active) return
    record.active = false
    this.#admissions.delete(record)
    this.#finishCloseIfDrained()
  }

  #finishCloseIfDrained(): void {
    if (this.#accepting || this.#admissions.size !== 0 || this.#pendingCloseRequests !== 0) return
    const resolve = this.#resolveClose
    const reject = this.#rejectClose
    if (resolve === undefined || reject === undefined) return
    this.#resolveClose = undefined
    this.#rejectClose = undefined
    if (this.#closeFailures.length === 0) {
      resolve()
      return
    }
    reject(new AggregateError(
      this.#closeFailures,
      'Persistent tree file transactions did not close cleanly',
    ))
  }
}
