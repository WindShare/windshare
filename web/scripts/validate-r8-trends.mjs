import { readFile } from 'node:fs/promises'

import { validateR8TrendRecords } from './r8-trend-contract.mjs'

const [recordsPath, sampleCountText] = process.argv.slice(2)
if (recordsPath === undefined || sampleCountText === undefined) {
  throw new Error('Usage: validate-r8-trends.mjs <records.json> <sample-count>')
}
const sampleCount = Number(sampleCountText)
const records = JSON.parse(await readFile(recordsPath, 'utf8'))
const result = validateR8TrendRecords(records, sampleCount)
process.stdout.write(`${JSON.stringify({ schema: 1, status: 'Success', ...result })}\n`)
