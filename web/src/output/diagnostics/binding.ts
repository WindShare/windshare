import type {
  OutputFailureObservation,
  OutputFailureSink,
  OutputFailureSinks,
  OutputFailureStage,
} from './facts'

export interface OutputFailureBindingLease {
  revoke(): void
}

export interface OutputFailureBinding {
  readonly sinks: OutputFailureSinks
  bind(sinks: OutputFailureSinks | undefined): OutputFailureBindingLease
}

type SinkProperty =
  | 'outputReservation'
  | 'outputWrite'
  | 'outputCommit'
  | 'checkpoint'
  | 'settlement'
  | 'publication'
  | 'continuation'
  | 'reopen'
  | 'cleanup'

/**
 * One output runtime spans several presentation attempts. This runtime-local switch
 * lets each operation lend its own sinks without introducing an ambient page scope.
 */
export function createOutputFailureBinding(
  initial?: OutputFailureSinks,
): OutputFailureBinding {
  let nextToken = 0
  let current: Readonly<{ token: number; sinks: OutputFailureSinks }> | undefined
  const bind = (sinks: OutputFailureSinks | undefined): OutputFailureBindingLease => {
    const token = ++nextToken
    current = sinks === undefined ? undefined : Object.freeze({ token, sinks })
    return Object.freeze({
      revoke: () => {
        if (current?.token === token) current = undefined
      },
    })
  }
  const proxy = <Stage extends OutputFailureStage>(
    property: SinkProperty,
    stage: Stage,
  ): OutputFailureSink<Stage> => Object.freeze({
    stage,
    record: (observation: OutputFailureObservation<Stage>) => {
      const active = current?.sinks[property] as OutputFailureSink<Stage> | undefined
      return active?.record(observation)
    },
  })
  const sinks: OutputFailureSinks = Object.freeze({
    outputReservation: proxy('outputReservation', 'output_reservation'),
    outputWrite: proxy('outputWrite', 'output_write'),
    outputCommit: proxy('outputCommit', 'output_commit'),
    checkpoint: proxy('checkpoint', 'checkpoint'),
    settlement: proxy('settlement', 'settlement'),
    publication: proxy('publication', 'publication'),
    continuation: proxy('continuation', 'continuation'),
    reopen: proxy('reopen', 'reopen'),
    cleanup: proxy('cleanup', 'cleanup'),
  })
  if (initial !== undefined) bind(initial)
  return Object.freeze({ sinks, bind })
}
