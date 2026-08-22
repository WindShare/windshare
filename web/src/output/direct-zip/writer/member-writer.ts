interface DirectZipMemberWriterOwner {
  writeMember(handleGeneration: number, bytes: Uint8Array): Promise<void>
  closeMember(handleGeneration: number): Promise<void>
}

/** The generation token prevents a restored checkpoint from accepting a stale member handle. */
export class DirectZipMemberWriterV1 {
  readonly #owner: DirectZipMemberWriterOwner
  readonly #generation: number

  constructor(owner: DirectZipMemberWriterOwner, generation: number) {
    this.#owner = owner
    this.#generation = generation
  }

  write(bytes: Uint8Array): Promise<void> {
    return this.#owner.writeMember(this.#generation, bytes)
  }

  close(): Promise<void> {
    return this.#owner.closeMember(this.#generation)
  }
}
