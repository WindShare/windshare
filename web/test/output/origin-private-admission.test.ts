import { describe, expect, it } from 'vitest'

import {
  MINIMUM_OPFS_QUOTA_RESERVE,
  OriginPrivateStagingAdmission,
} from '../../src/output/origin-private/admission'
import type {
  AdmissionAggregateLimits,
  AdmissionLeaseRecord,
  OriginPrivateAdmissionAuthority,
} from '../../src/output/origin-private/admission-authority'

const MEBIBYTE = 1024n * 1024n

describe('origin-private staging admission', () => {
  it('reserves browser headroom in addition to the job limit', async () => {
    const database = new MemoryAdmissionDatabase()
    const quota = MINIMUM_OPFS_QUOTA_RESERVE + 100n * MEBIBYTE
    const admission = await OriginPrivateStagingAdmission.open('quota-session', emptyTotals(), {
      estimate: async () => ({ quota: Number(quota), usage: 0 }),
      authority: database.open(),
    })
    await expect(admission.reserve(['too-large'], 101n * MEBIBYTE, emptyFootprint()))
      .rejects.toThrow('shared browser quota reserve')
    await expect(admission.reserve(['fits'], 100n * MEBIBYTE, emptyFootprint()))
      .resolves.toBeTypeOf('function')
    await admission.release()
  })

  it('shares the process ceiling across jobs and releases failed-file reservations', async () => {
    const estimate = async () => ({ quota: Number(2n * 1024n * MEBIBYTE), usage: 0 })
    const database = new MemoryAdmissionDatabase()
    const first = await OriginPrivateStagingAdmission.open('process-a', emptyTotals(), {
      estimate,
      jobLimit: 10n,
      processLimit: 10n,
      authority: database.open(),
    })
    const second = await OriginPrivateStagingAdmission.open('process-b', emptyTotals(), {
      estimate,
      jobLimit: 10n,
      processLimit: 10n,
      authority: database.open(),
    })
    await first.reserve(['a'], 6n, emptyFootprint())
    await expect(second.reserve(['b'], 5n, emptyFootprint())).rejects.toThrow('process limit')
    await first.releaseFile(['a'])
    await expect(second.reserve(['b'], 5n, emptyFootprint())).resolves.toBeTypeOf('function')
    await first.release()
    await second.release()
  })

  it('can roll back a reservation when file creation never begins', async () => {
    const database = new MemoryAdmissionDatabase()
    const admission = await OriginPrivateStagingAdmission.open('rollback', emptyTotals(), {
      estimate: async () => ({ quota: Number(2n * 1024n * MEBIBYTE), usage: 0 }),
      jobLimit: 5n,
      processLimit: 5n,
      authority: database.open(),
    })
    const rollback = await admission.reserve(['first'], 5n, emptyFootprint())
    await rollback()
    await expect(admission.reserve(['second'], 5n, emptyFootprint())).resolves.toBeTypeOf('function')
    await admission.release()
  })

  it('atomically fences a stale lease when the same locked session recovers', async () => {
    const database = new MemoryAdmissionDatabase()
    const options = {
      estimate: async () => ({ quota: Number(2n * 1024n * MEBIBYTE), usage: 0 }),
      jobLimit: 10n,
      processLimit: 10n,
    }
    const stale = await OriginPrivateStagingAdmission.open('recovering-session', emptyTotals(), {
      ...options,
      authority: database.open(),
      randomToken: () => 'stale-token',
    })
    const recovered = await OriginPrivateStagingAdmission.open('recovering-session', emptyTotals(), {
      ...options,
      authority: database.open(),
      randomToken: () => 'recovered-token',
    })

    await expect(stale.reserve(['stale'], 1n, emptyFootprint())).rejects.toThrow('lease changed')
    await expect(recovered.reserve(['recovered'], 1n, emptyFootprint()))
      .resolves.toBeTypeOf('function')
    await stale.release()
    await recovered.release()
    expect(database.records.size).toBe(0)
  })

  it('rejects duplicate active paths without corrupting scalar recovery credit', async () => {
    const database = new MemoryAdmissionDatabase()
    const admission = await OriginPrivateStagingAdmission.open('duplicate-active', {
      logicalBytes: 10n,
      additionalBytes: 6n,
    }, {
      estimate: async () => ({ quota: Number(2n * 1024n * MEBIBYTE), usage: 4 }),
      jobLimit: 20n,
      processLimit: 20n,
      authority: database.open(),
    })
    const rollback = await admission.reserve(['file'], 10n, {
      logicalBytes: 10n,
      coveredBytes: 4n,
    })
    await expect(admission.reserve(['file'], 10n, {
      logicalBytes: 10n,
      coveredBytes: 4n,
    })).rejects.toThrow('active reservation')
    await rollback()
    expect(admission.snapshot()).toEqual({
      logicalBytes: 10n,
      additionalBytes: 6n,
      activeReservations: 0,
    })
    await admission.release()
  })

  it('represents a million staged small files with scalars and only bounded active reservations', async () => {
    const million = 1_000_000n
    const database = new MemoryAdmissionDatabase()
    const admission = await OriginPrivateStagingAdmission.open('million-files', {
      logicalBytes: million,
      additionalBytes: 0n,
    }, {
      estimate: async () => ({ quota: Number(2n * 1024n * MEBIBYTE), usage: Number(million) }),
      jobLimit: 2n * million,
      processLimit: 2n * million,
      authority: database.open(),
    })

    expect(admission.snapshot()).toEqual({
      logicalBytes: million,
      additionalBytes: 0n,
      activeReservations: 0,
    })
    for (let index = 0; index < 32; index += 1) {
      await admission.reserve([`active-${index}`], 1n, emptyFootprint())
    }
    expect(admission.snapshot().activeReservations).toBe(32)
    await expect(admission.reserve(['overflow'], 1n, emptyFootprint()))
      .rejects.toThrow('active reservation limit')
    await admission.release()
  })
})

function emptyTotals() {
  return { logicalBytes: 0n, additionalBytes: 0n }
}

function emptyFootprint() {
  return { logicalBytes: 0n, coveredBytes: 0n }
}

class MemoryAdmissionDatabase {
  readonly records = new Map<string, AdmissionLeaseRecord>()

  open(): OriginPrivateAdmissionAuthority {
    return new MemoryAdmissionAuthority(this.records)
  }
}

class MemoryAdmissionAuthority implements OriginPrivateAdmissionAuthority {
  readonly #records: Map<string, AdmissionLeaseRecord>
  #closed = false

  constructor(records: Map<string, AdmissionLeaseRecord>) {
    this.#records = records
  }

  async claim(record: AdmissionLeaseRecord, limits: AdmissionAggregateLimits): Promise<void> {
    this.#set(record, limits)
  }

  async update(record: AdmissionLeaseRecord, limits: AdmissionAggregateLimits): Promise<void> {
    const existing = this.#records.get(record.id)
    if (existing?.token !== record.token) throw new Error('lease changed')
    this.#set(record, limits)
  }

  async heartbeat(
    id: string,
    token: string,
    expiresAtMilliseconds: number,
    nowMilliseconds: number,
  ): Promise<void> {
    const existing = this.#records.get(id)
    if (existing?.token !== token || existing.expiresAtMilliseconds <= nowMilliseconds) {
      throw new Error('lease changed')
    }
    this.#records.set(id, { ...existing, expiresAtMilliseconds })
  }

  async release(id: string, token: string): Promise<void> {
    if (this.#records.get(id)?.token === token) this.#records.delete(id)
  }

  close(): void {
    this.#closed = true
  }

  #set(record: AdmissionLeaseRecord, limits: AdmissionAggregateLimits): void {
    if (this.#closed) throw new Error('authority closed')
    let logicalBytes = record.logicalBytes
    let additionalBytes = record.additionalBytes
    for (const [id, existing] of this.#records) {
      if (id === record.id || existing.expiresAtMilliseconds <= limits.nowMilliseconds) continue
      logicalBytes += existing.logicalBytes
      additionalBytes += existing.additionalBytes
    }
    if (record.logicalBytes > limits.jobLimit) throw new DOMException('per-job limit', 'QuotaExceededError')
    if (logicalBytes > limits.processLimit) throw new DOMException('process limit', 'QuotaExceededError')
    if (limits.usage + additionalBytes + limits.reserve > limits.quota) {
      throw new DOMException('shared browser quota reserve', 'QuotaExceededError')
    }
    this.#records.set(record.id, record)
  }
}
