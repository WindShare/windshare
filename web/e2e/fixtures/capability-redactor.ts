// Keep the fixture import path stable while the implementation remains in the
// browser-safe security module. This lets Playwright diagnostics and the
// receiver controller share exactly the same actual-value replacement rules.
export {
  capabilityRedactionValuesFromInput,
  createCapabilityRedactor,
  formatCapabilityDiagnostic,
  redactCapabilityText,
  redactCapabilityValue,
  withCapabilityRedaction,
} from '../../src/security/capability-redactor'
export type {
  CapabilityRedactionValues,
  CapabilityRedactor,
} from '../../src/security/capability-redactor'
