import { describe, expect, it } from 'vitest'

import {
  SMALL_TRANSFER_BYTE_LIMIT,
  SMALL_TRANSFER_FILE_LIMIT,
  SelectionMeasureTracker,
} from '../../src/transfer/measure'

const MILLION_UNIQUE_FILE_OBSERVATIONS = 1_000_000

describe('incremental selection measurement', () => {
  it('classifies a terminal empty selection as small', () => {
    expect(new SelectionMeasureTracker().complete()).toMatchObject({
      discoveredFiles: 0,
      discoveredBytes: 0n,
      discovery: 'complete',
      sizeClass: 'small',
    })
  })

  it('uses strict file-count boundaries and remains unknown before terminal discovery', () => {
    const measure = new SelectionMeasureTracker()
    for (let index = 0; index < SMALL_TRANSFER_FILE_LIMIT - 1; index += 1) {
      measure.observeUniqueFile(0n)
    }
    expect(measure.snapshot()).toMatchObject({
      discoveredFiles: 29,
      sizeClass: 'unknown',
      discovery: 'open',
    })
    expect(measure.complete()).toMatchObject({ sizeClass: 'small', discovery: 'complete' })

    const boundary = new SelectionMeasureTracker()
    for (let index = 0; index < SMALL_TRANSFER_FILE_LIMIT; index += 1) {
      boundary.observeUniqueFile(0n)
    }
    expect(boundary.snapshot()).toMatchObject({ sizeClass: 'large', discovery: 'open' })
  })

  it('uses strict byte boundaries', () => {
    const below = new SelectionMeasureTracker()
    below.observeUniqueFile(SMALL_TRANSFER_BYTE_LIMIT - 1n)
    expect(below.complete()).toMatchObject({ sizeClass: 'small' })

    const exact = new SelectionMeasureTracker()
    exact.observeUniqueFile(SMALL_TRANSFER_BYTE_LIMIT)
    expect(exact.snapshot()).toMatchObject({ sizeClass: 'large', discovery: 'open' })
  })

  it('keeps failed incomplete discovery non-small while preserving proven large state', () => {
    const unknown = new SelectionMeasureTracker()
    unknown.observeUniqueFile(1n)
    expect(unknown.fail()).toMatchObject({ sizeClass: 'unknown', discovery: 'failed' })

    const large = new SelectionMeasureTracker()
    large.observeUniqueFile(SMALL_TRANSFER_BYTE_LIMIT)
    expect(large.fail()).toMatchObject({ sizeClass: 'large', discovery: 'failed' })
  })

  it('streams a million unique observations without retaining identity ownership', () => {
    const measure = new SelectionMeasureTracker()
    for (let index = 0; index < MILLION_UNIQUE_FILE_OBSERVATIONS; index += 1) {
      measure.observeUniqueFile(0n)
    }
    expect(measure.snapshot()).toMatchObject({
      discoveredFiles: MILLION_UNIQUE_FILE_OBSERVATIONS,
      discoveredBytes: 0n,
      sizeClass: 'large',
    })
    measure.complete()
    expect(() => measure.observeUniqueFile(0n)).toThrow(/terminal/u)
  })
})
