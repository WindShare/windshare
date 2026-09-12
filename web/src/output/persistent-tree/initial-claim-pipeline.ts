interface ClaimInput {
  readonly key: string
  readonly lineageId: string
}

interface ClaimWork<TInput, TInspection, TDecision> {
  readonly input: TInput
  readonly promise: Promise<TDecision>
  readonly resolve: (decision: TDecision) => void
  readonly reject: (error: unknown) => void
  state: 'pending' | 'classifying' | 'inspection' | 'inspecting' | 'ready' | 'settling'
  inspection?: TInspection
}

export interface InitialClaimPipelineOptions<TInput extends ClaimInput, TInspection, TDecision> {
  readonly maximumResident: number
  readonly maximumInspecting: number
  readonly classify: (inputs: readonly TInput[]) => Promise<readonly (TDecision | undefined)[]>
  readonly inspect: (input: TInput) => Promise<TInspection>
  readonly settle: (work: readonly Readonly<{ input: TInput; inspection: TInspection }>[]) =>
    Promise<readonly TDecision[]>
  readonly admitted?: (input: TInput) => void
  readonly completed?: (input: TInput, succeeded: boolean) => void
  readonly observe?: (state: Readonly<{ active: number; queuedMembers: number; pendingMembers: number }>) => void
  readonly drained?: () => void
}

/**
 * Journal transactions batch whatever is ready; their batch membership never owns
 * inspection slots. Residence includes ready results so a slow journal pushes back
 * on admission instead of accumulating inspected destinations in memory.
 */
export class InitialClaimPipeline<TInput extends ClaimInput, TInspection, TDecision> {
  readonly #options: InitialClaimPipelineOptions<TInput, TInspection, TDecision>
  readonly #work = new Map<string, ClaimWork<TInput, TInspection, TDecision>>()
  readonly #lineages = new Set<string>()
  #resident = 0
  #inspecting = 0
  #journalActive = false
  #scheduled = false

  constructor(options: InitialClaimPipelineOptions<TInput, TInspection, TDecision>) {
    if (!Number.isSafeInteger(options.maximumResident) || options.maximumResident < 1 ||
        !Number.isSafeInteger(options.maximumInspecting) || options.maximumInspecting < 1 ||
        options.maximumInspecting > options.maximumResident) {
      throw new TypeError('initial claim pipeline capacity is invalid')
    }
    this.#options = options
  }

  select(input: TInput): Promise<TDecision> {
    const existing = this.#work.get(input.key)
    if (existing !== undefined) return existing.promise
    let resolve!: (decision: TDecision) => void
    let reject!: (error: unknown) => void
    const promise = new Promise<TDecision>((accept, fail) => { resolve = accept; reject = fail })
    this.#work.set(input.key, {
      input, promise, resolve, reject, state: 'pending',
    })
    this.#schedule()
    return promise
  }

  #schedule(): void {
    this.#observe()
    if (this.#scheduled) return
    this.#scheduled = true
    // A microtask collects synchronous arrivals/completions without imposing a
    // first-file timer or waiting for another inspector to finish.
    queueMicrotask(() => {
      this.#scheduled = false
      this.#pump()
    })
  }

  #pump(): void {
    if (!this.#journalActive) this.#admitPending()
    this.#refillInspectors()
    if (!this.#journalActive) this.#startJournal()
    this.#observe()
    if (this.#work.size === 0) this.#observation(() => this.#options.drained?.())
  }

  #admitPending(): void {
    for (const work of this.#work.values()) {
      if (this.#resident >= this.#options.maximumResident) break
      if (work.state !== 'pending' || this.#lineages.has(work.input.lineageId)) continue
      this.#lineages.add(work.input.lineageId)
      this.#resident += 1
      work.state = 'classifying'
      this.#observation(() => this.#options.admitted?.(work.input))
    }
  }

  #refillInspectors(): void {
    for (const work of this.#work.values()) {
      if (this.#inspecting >= this.#options.maximumInspecting) break
      if (work.state !== 'inspection') continue
      this.#startInspection(work)
    }
  }

  #startJournal(): void {
    const ready = [...this.#work.values()].filter(work => work.state === 'ready')
    // Already classified claims retain their slots while ready commits run.
    if (ready.length !== 0) this.#startSettlement(ready)
    else {
      const classifying = [...this.#work.values()].filter(work => work.state === 'classifying')
      if (classifying.length !== 0) this.#startClassification(classifying)
    }
  }

  #startClassification(work: readonly ClaimWork<TInput, TInspection, TDecision>[]): void {
    this.#journalActive = true
    Promise.resolve().then(() => this.#options.classify(work.map(item => item.input))).then(
      decisions => {
        if (decisions.length !== work.length) throw new TypeError('initial claim classification cardinality changed')
        work.forEach((item, index) => {
          const decision = decisions[index]
          if (decision === undefined) item.state = 'inspection'
          else this.#complete(item, decision)
        })
      },
    ).catch((error: unknown) => work.forEach(item => this.#reject(item, error))).finally(() => {
      this.#journalActive = false
      this.#schedule()
    })
  }

  #startInspection(work: ClaimWork<TInput, TInspection, TDecision>): void {
    work.state = 'inspecting'
    this.#inspecting += 1
    Promise.resolve().then(() => this.#options.inspect(work.input)).then(inspection => {
      work.inspection = inspection
      work.state = 'ready'
    }).catch((error: unknown) => this.#reject(work, error)).finally(() => {
      this.#inspecting -= 1
      this.#schedule()
    })
  }

  #startSettlement(work: readonly ClaimWork<TInput, TInspection, TDecision>[]): void {
    this.#journalActive = true
    work.forEach(item => { item.state = 'settling' })
    Promise.resolve().then(() => this.#options.settle(work.map(item => ({
      input: item.input,
      inspection: item.inspection!,
    })))).then(decisions => {
      if (decisions.length !== work.length) throw new TypeError('initial claim settlement cardinality changed')
      work.forEach((item, index) => this.#complete(item, decisions[index]!))
    }).catch((error: unknown) => work.forEach(item => this.#reject(item, error))).finally(() => {
      this.#journalActive = false
      this.#schedule()
    })
  }

  #complete(work: ClaimWork<TInput, TInspection, TDecision>, decision: TDecision): void {
    this.#work.delete(work.input.key)
    this.#lineages.delete(work.input.lineageId)
    this.#resident -= 1
    this.#observation(() => this.#options.completed?.(work.input, true))
    work.resolve(decision)
  }

  #reject(work: ClaimWork<TInput, TInspection, TDecision>, error: unknown): void {
    // Each promise owns its native/journal call until that call settles. A failed
    // file can then release its admission without cancelling unrelated siblings.
    this.#work.delete(work.input.key)
    this.#lineages.delete(work.input.lineageId)
    this.#resident -= 1
    this.#observation(() => this.#options.completed?.(work.input, false))
    work.reject(error)
  }

  #observe(): void {
    let queuedMembers = 0
    let pendingMembers = 0
    for (const work of this.#work.values()) {
      if (work.state === 'inspection') queuedMembers += 1
      if (work.state === 'pending' || work.state === 'classifying') pendingMembers += 1
    }
    this.#observation(() => this.#options.observe?.({ active: this.#inspecting, queuedMembers, pendingMembers }))
  }

  #observation(observe: () => void): void {
    try { observe() } catch {
      // Measurements cannot acquire scheduling or failure authority.
    }
  }
}
