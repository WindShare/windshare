import { resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

import type { NetworkMatrixBrowser } from '../vocabulary.ts'
import {
  loadContainedBrowserSampleSecret,
  runContainedBrowserSample,
  type ContainedBrowserSampleOutput,
} from './contained-browser-sample.ts'

const CHILD_FAILURE_MESSAGE = 'contained browser network matrix child failed'

export interface ContainedBrowserSampleChildComposition {
  readonly loadSecret?: typeof loadContainedBrowserSampleSecret
  readonly run?: typeof runContainedBrowserSample
  readonly writeOutput?: (encoded: string) => void
}

export async function containedBrowserSampleChild(
  arguments_: readonly string[],
  composition: ContainedBrowserSampleChildComposition = {},
): Promise<ContainedBrowserSampleOutput> {
  const { browser } = parseArguments(arguments_)
  const controller = new AbortController()
  const terminate = (): void => controller.abort()
  process.once('SIGTERM', terminate)
  process.once('SIGINT', terminate)
  let secret: Awaited<ReturnType<typeof loadContainedBrowserSampleSecret>> | undefined
  try {
    secret = await (composition.loadSecret ?? loadContainedBrowserSampleSecret)(process.stdin)
    const output = await (composition.run ?? runContainedBrowserSample)({
      browser,
      secret,
      signal: controller.signal,
    })
    ;(composition.writeOutput ?? ((encoded: string) => process.stdout.write(encoded)))(
      `${JSON.stringify(output)}\n`,
    )
    return output
  } finally {
    // RemotePionControlClient intentionally aliases this byte-owned buffer.
    // The contained process is the last honest mutable boundary, so it erases
    // the authority after browser/control cleanup and on output-write failure.
    secret?.control.credential.fill(0)
    process.removeListener('SIGTERM', terminate)
    process.removeListener('SIGINT', terminate)
  }
}

function parseArguments(arguments_: readonly string[]): {
  readonly browser: NetworkMatrixBrowser
} {
  if (arguments_.length !== 2 || arguments_[0] !== '--browser') {
    throw new Error(CHILD_FAILURE_MESSAGE)
  }
  const browser = arguments_[1]
  if (browser !== 'chromium' && browser !== 'firefox' && browser !== 'webkit') {
    throw new Error(CHILD_FAILURE_MESSAGE)
  }
  return Object.freeze({ browser })
}

const invokedPath = process.argv[1]
if (
  invokedPath !== undefined &&
  pathToFileURL(resolve(invokedPath)).href === pathToFileURL(fileURLToPath(import.meta.url)).href
) {
  containedBrowserSampleChild(process.argv.slice(2)).then(
    () => { process.exitCode = 0 },
    () => {
      // A constant diagnostic makes every rejected secret/configuration branch
      // non-reflective even when a lower layer received credential-bearing input.
      process.stderr.write(`${CHILD_FAILURE_MESSAGE}\n`)
      process.exitCode = 1
    },
  )
}
