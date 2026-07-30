export interface BrowserVerdictSuiteInput {
  readonly root: string
  readonly jobOutcome: string
  readonly guardOutcome: string
  readonly downloadOutcome: string
  readonly manifestSha256: string
  readonly manifestByteLength: string
}

export interface BrowserVerdictInput {
  readonly runId: string
  readonly checkoutSha: string
  readonly suites: Readonly<{
    main: BrowserVerdictSuiteInput
    pion: BrowserVerdictSuiteInput
  }>
}

export interface BrowserGateVerdict {
  readonly verdict: 'passed' | 'failed'
  readonly violations: readonly string[]
  readonly topologyAuthority: Readonly<Record<string, unknown>>
  readonly samples: readonly Readonly<Record<string, unknown>>[]
}

export function evaluateBrowserGate(options: BrowserVerdictInput): Promise<BrowserGateVerdict>
export function runBrowserVerdictCli(argv?: readonly string[]): Promise<number>
