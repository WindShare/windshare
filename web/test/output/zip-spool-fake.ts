import type {
  ZipCentralDirectoryManifest,
  ZipCentralDirectorySpool,
} from '../../src/output/streams/zip-spool'

export class MemoryZipCentralDirectorySpool implements ZipCentralDirectorySpool {
  readonly #records: Uint8Array[] = []
  #chunks: Uint8Array[] = []
  #sealed = false
  cleared = false

  async append(record: Uint8Array): Promise<void> {
    if (this.#sealed || this.cleared) throw new Error('test ZIP spool is settled')
    this.#records.push(record.slice())
  }

  async seal(): Promise<ZipCentralDirectoryManifest> {
    if (!this.#sealed) {
      this.#sealed = true
      this.#chunks = this.#records.map((record) => record.slice())
      this.#records.length = 0
    }
    return {
      chunkCount: this.#chunks.length,
      recordCount: BigInt(this.#chunks.length),
      byteLength: this.#chunks.reduce((total, chunk) => total + BigInt(chunk.byteLength), 0n),
    }
  }

  async readChunk(index: number): Promise<Uint8Array | undefined> {
    return this.#chunks[index]?.slice()
  }

  async clear(): Promise<void> {
    this.cleared = true
    this.#records.length = 0
    this.#chunks.length = 0
  }
}
