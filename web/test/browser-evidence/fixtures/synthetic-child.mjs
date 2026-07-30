import { spawn } from 'node:child_process'
import { isAbsolute, join, resolve } from 'node:path'

import { readChildEvidenceContext } from '../../../scripts/browser-evidence/child-evidence.ts'
import { runSyntheticScenario } from '../synthetic-scenario.ts'

const mode = process.env.SYNTHETIC_CHILD_MODE ?? 'main-unavailable'
const delayMs = Number(process.env.SYNTHETIC_CHILD_DELAY_MS ?? '0')
const LATE_RESULT_WRITE_DELAY_MS = 600
const ROOT_EXIT_DESCENDANT_WRITE_DELAY_MS = 1_200
const DOUBLE_FORK_HANDSHAKE_DEADLINE_MS = 5_000

if (mode === 'descendant-timeout') spawnLateResultWriter(true)
if (mode === 'root-exit-with-descendant') {
  spawnLateResultWriter(true, ROOT_EXIT_DESCENDANT_WRITE_DELAY_MS)
}
if (mode === 'descendant-after-root') spawnLateResultWriter(false)
if (mode === 'detached-after-root') spawnLateResultWriter(false, LATE_RESULT_WRITE_DELAY_MS, true)
if (mode === 'double-fork-after-root') await spawnDoubleForkLateResultWriter()

const outcome = await runSyntheticScenario({
  context: readChildEvidenceContext(),
  mode,
  delayMs,
  environment: process.env,
  stdout: (chunk) => process.stdout.write(chunk),
  stderr: (chunk) => process.stderr.write(chunk),
})
process.exitCode = outcome.exitCode

function spawnLateResultWriter(
  keepAlive,
  mutationDelayMs = LATE_RESULT_WRITE_DELAY_MS,
  detached = process.platform === 'win32',
) {
  const { resultPath, markerPath, writerScript } = lateWriterFixture(mutationDelayMs, keepAlive)
  const descendant = spawn(process.execPath, ['-e', writerScript, resultPath, markerPath], {
    detached,
    stdio: 'ignore',
  })
  descendant.unref()
}

async function spawnDoubleForkLateResultWriter() {
  const { resultPath, markerPath, writerScript } = lateWriterFixture(
    LATE_RESULT_WRITE_DELAY_MS,
    false,
  )
  const launcherScript = [
    "const { spawn } = require('node:child_process')",
    "const descendant = spawn(process.execPath, ['-e', process.argv[1], process.argv[2], process.argv[3]], { detached: true, stdio: 'ignore' })",
    'descendant.unref()',
    "process.send('descendant-started')",
    'process.disconnect()',
  ].join(';')
  const launcher = spawn(
    process.execPath,
    ['-e', launcherScript, writerScript, resultPath, markerPath],
    { detached: true, stdio: ['ignore', 'ignore', 'ignore', 'ipc'] },
  )
  await waitForDoubleForkHandshake(launcher)
  launcher.unref()
}

function waitForDoubleForkHandshake(launcher) {
  return new Promise((resolveHandshake, rejectHandshake) => {
    const deadline = setTimeout(() => {
      rejectHandshake(new Error('double-fork descendant startup handshake timed out'))
    }, DOUBLE_FORK_HANDSHAKE_DEADLINE_MS)
    launcher.once('error', rejectHandshake)
    launcher.once('exit', (code, signal) => {
      rejectHandshake(new Error(
        `double-fork launcher exited before its descendant handshake: code=${code}; signal=${signal}`,
      ))
    })
    launcher.once('message', (message) => {
      if (message !== 'descendant-started') {
        rejectHandshake(new Error('double-fork launcher returned an invalid descendant handshake'))
        return
      }
      clearTimeout(deadline)
      resolveHandshake()
    })
  })
}

function lateWriterFixture(mutationDelayMs, keepAlive) {
  const context = readChildEvidenceContext()
  const resultPath = explicitFinalResultPath()
  const markerPath = join(context.artifactRoot, 'descendant-ran.txt')
  const writerScript = [
    "const { writeFile } = require('node:fs/promises')",
    `setTimeout(async () => { await writeFile(process.argv[2], 'ran'); await writeFile(process.argv[1], '{\"lateWriter\":true}\\n').catch(() => undefined) }, ${mutationDelayMs})`,
    // A keepalive models a deadline survivor; finite escaped writers model the
    // publication race after their root has already reported success.
    ...(keepAlive ? ['setInterval(() => {}, 60000)'] : []),
  ].join(';')
  return { resultPath, markerPath, writerScript }
}

function explicitFinalResultPath() {
  const resultPath = process.env.SYNTHETIC_FINAL_RESULT_PATH
  if (resultPath === undefined || !isAbsolute(resultPath) || resolve(resultPath) !== resultPath) {
    throw new Error('SYNTHETIC_FINAL_RESULT_PATH must be an explicit absolute canonical path')
  }
  return resultPath
}
