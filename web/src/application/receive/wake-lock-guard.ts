export interface ScreenWakeLockSentinelLike {
  readonly released?: boolean
  release(): Promise<void>
  addEventListener?(type: 'release', listener: () => void): void
  removeEventListener?(type: 'release', listener: () => void): void
}

export interface ScreenWakeLockManagerLike {
  request(type: 'screen'): Promise<ScreenWakeLockSentinelLike>
}

export interface WakeLockDocumentLike {
  readonly visibilityState?: DocumentVisibilityState
  addEventListener?(type: 'visibilitychange', listener: () => void): void
  removeEventListener?(type: 'visibilitychange', listener: () => void): void
}

export interface ScreenWakeLockWindowTarget {
  readonly navigator?: {
    readonly wakeLock?: ScreenWakeLockManagerLike
  }
  readonly document?: WakeLockDocumentLike
}

/**
 * Dynamically holds a Screen Wake Lock while a download session is actively
 * running on a visible document so unattended browser transfers avoid idle
 * sleep, releasing immediately on pause, completion, failure, or disposal.
 */
export class ScreenWakeLockGuard {
  readonly #target: ScreenWakeLockWindowTarget | undefined
  #wakeLock: ScreenWakeLockManagerLike | undefined
  #document: WakeLockDocumentLike | undefined
  #armed = false
  #disposed = false
  #generation = 0
  #pendingGeneration: number | undefined
  #sentinel: ScreenWakeLockSentinelLike | undefined
  #detachSentinelRelease: (() => void) | undefined

  constructor(target: ScreenWakeLockWindowTarget | undefined) {
    this.#target = target
  }

  get armed(): boolean {
    return this.#armed
  }

  setArmed(armed: boolean): void {
    if (this.#disposed || this.#armed === armed) return
    if (armed) {
      const wakeLock = resolveWakeLock(this.#target)
      if (wakeLock === undefined) return
      this.#wakeLock = wakeLock
      this.#document = resolveDocument(this.#target)
      this.#armed = true
      this.#document?.addEventListener?.('visibilitychange', this.#handleVisibilityChange)
      this.#acquire()
      return
    }
    this.#armed = false
    this.#generation += 1
    this.#pendingGeneration = undefined
    this.#document?.removeEventListener?.('visibilitychange', this.#handleVisibilityChange)
    this.#releaseCurrent()
  }

  dispose(): void {
    if (this.#disposed) return
    this.setArmed(false)
    this.#disposed = true
  }

  readonly #handleVisibilityChange = (): void => {
    if (!this.#armed || this.#disposed) return
    if (this.#document?.visibilityState === 'hidden') return
    this.#acquire()
  }

  #acquire(): void {
    if (!this.#armed || this.#disposed || this.#wakeLock === undefined) return
    if (this.#document?.visibilityState === 'hidden') return
    if (this.#sentinel !== undefined && !this.#sentinel.released) return
    if (this.#pendingGeneration === this.#generation) return

    const generation = this.#generation
    this.#pendingGeneration = generation
    let request: Promise<ScreenWakeLockSentinelLike>
    try {
      request = this.#wakeLock.request('screen')
    } catch {
      this.#pendingGeneration = undefined
      return
    }
    void request.then(
      sentinel => this.#acceptSentinel(generation, sentinel),
      () => {
        if (this.#pendingGeneration === generation) this.#pendingGeneration = undefined
      },
    )
  }

  #acceptSentinel(generation: number, sentinel: ScreenWakeLockSentinelLike): void {
    if (this.#pendingGeneration === generation) this.#pendingGeneration = undefined
    if (this.#disposed || !this.#armed || generation !== this.#generation) {
      void sentinel.release().catch(() => undefined)
      return
    }
    this.#releaseCurrent()
    this.#sentinel = sentinel
    const detach = (): void => {
      sentinel.removeEventListener?.('release', onRelease)
    }
    const onRelease = (): void => {
      detach()
      if (this.#sentinel !== sentinel) return
      this.#sentinel = undefined
      this.#detachSentinelRelease = undefined
    }
    sentinel.addEventListener?.('release', onRelease)
    this.#detachSentinelRelease = detach
  }

  #releaseCurrent(): void {
    const sentinel = this.#sentinel
    const detach = this.#detachSentinelRelease
    this.#sentinel = undefined
    this.#detachSentinelRelease = undefined
    detach?.()
    if (sentinel !== undefined && !sentinel.released) {
      void sentinel.release().catch(() => undefined)
    }
  }
}

function resolveWakeLock(
  target: ScreenWakeLockWindowTarget | undefined,
): ScreenWakeLockManagerLike | undefined {
  try {
    const wakeLock = target?.navigator?.wakeLock
    return typeof wakeLock?.request === 'function' ? wakeLock : undefined
  } catch {
    return undefined
  }
}

function resolveDocument(
  target: ScreenWakeLockWindowTarget | undefined,
): WakeLockDocumentLike | undefined {
  try {
    return target?.document
  } catch {
    return undefined
  }
}
