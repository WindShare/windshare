export interface BeforeUnloadEventLike {
  preventDefault(): void
  returnValue?: unknown
}

export interface BeforeUnloadEventTarget {
  addEventListener(type: 'beforeunload', listener: (event: BeforeUnloadEventLike) => void): void
  removeEventListener(type: 'beforeunload', listener: (event: BeforeUnloadEventLike) => void): void
}

/**
 * Dynamically registers a `beforeunload` guard only while a download is
 * actively running or settling, keeping idle and paused documents free of
 * unload listeners so browser bfcache remains available.
 */
export class BeforeUnloadGuard {
  readonly #target: BeforeUnloadEventTarget | undefined
  #armed = false
  #disposed = false

  constructor(target: Partial<BeforeUnloadEventTarget> | undefined) {
    this.#target = isBeforeUnloadTarget(target) ? target : undefined
  }

  get armed(): boolean {
    return this.#armed
  }

  setArmed(armed: boolean): void {
    if (this.#disposed || this.#target === undefined || this.#armed === armed) return
    if (armed) {
      this.#target.addEventListener('beforeunload', this.#handleBeforeUnload)
      this.#armed = true
      return
    }
    this.#target.removeEventListener('beforeunload', this.#handleBeforeUnload)
    this.#armed = false
  }

  dispose(): void {
    if (this.#disposed) return
    this.setArmed(false)
    this.#disposed = true
  }

  readonly #handleBeforeUnload = (event: BeforeUnloadEventLike): void => {
    event.preventDefault()
    // Chromium still requires setting returnValue to trigger the native leave prompt.
    event.returnValue = ''
  }
}

function isBeforeUnloadTarget(
  candidate: Partial<BeforeUnloadEventTarget> | undefined,
): candidate is BeforeUnloadEventTarget {
  return typeof candidate?.addEventListener === 'function' &&
    typeof candidate.removeEventListener === 'function'
}
