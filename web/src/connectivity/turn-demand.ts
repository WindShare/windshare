import type { LaneSet } from '../content/lane-set'
import type { TURNMode } from '../protocol/turn-control'
import { THROUGHPUT_SAMPLE_MS } from './peer-set/throughput'

export type TURNContentDemand = ReturnType<LaneSet['schedulingDemand']>

const LARGE_TRANSFER_BYTES = 4 * 1024 * 1024
const WARMUP_MS = 2000
const STALL_MS = 3000
const RATE_WINDOW_MS = 5000
const SLOW_BYTES_PER_SECOND = 1024 * 1024

interface Sample { at: number; useful: number }
interface Pressure { started: number; progress: number; latest: Sample; samples: Sample[] }

export class TURNDeliveryPressure {
  #pressure: Pressure | undefined

  observe(now: number, demand: TURNContentDemand): TURNMode | undefined {
    const fullWindow = demand.queuedBytes === 0 && demand.inFlightBytes > 0 &&
      demand.remainingBytes > demand.inFlightBytes && demand.capacityWaitingBytes === 0
    if (!fullWindow && demand.networkWaitingBytes === 0) {
      this.reset()
      return
    }

    const sample = { at: now, useful: demand.usefulBytes }
    const pressure = this.#pressure ??= { started: now, progress: now, latest: sample, samples: [sample] }
    if (now > pressure.latest.at) {
      if (sample.useful > pressure.latest.useful) pressure.progress = now
      pressure.latest = sample
      // Useful bytes arrive in complete blocks. Keep one observation per sampling interval
      // and the closest baseline at or before the cutoff, including across delayed ticks.
      const samples = pressure.samples
      if (now - samples.at(-1)!.at >= THROUGHPUT_SAMPLE_MS) samples.push(sample)
      while (samples.length > 1 && samples[1]!.at <= now - RATE_WINDOW_MS) samples.shift()
    }
    const latest = pressure.latest
    if (demand.remainingBytes <= LARGE_TRANSFER_BYTES && latest.at - pressure.started <= WARMUP_MS) return
    const baseline = pressure.samples[0]!
    const elapsed = latest.at - baseline.at
    const undersupplied = elapsed >= RATE_WINDOW_MS &&
      Math.max(0, latest.useful - baseline.useful) * 1000 < SLOW_BYTES_PER_SECOND * elapsed
    return latest.at - pressure.progress >= STALL_MS || undersupplied ? 'all' : 'parallel'
  }

  reset(): void { this.#pressure = undefined }
}
