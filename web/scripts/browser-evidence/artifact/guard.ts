export {
  type ArtifactGuardScanHooks,
  type ArtifactGuardSuiteHooks,
  type ArtifactGuardTraceEvent,
  type ArtifactGuardTraceSink,
  type ExplicitGuardSecret,
  type GuardArtifactSuiteOptions,
  type GuardArtifactSuiteResult,
  type GuardArtifactSuiteSample,
} from './guard/contract.ts'
export { guardArtifactSuite, scanSampleArtifacts } from './guard/orchestrator.ts'
