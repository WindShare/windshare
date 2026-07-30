import { createGeneratedSemanticFailure } from './failure.mjs'

export const GENERATED_SEMANTIC_USAGE =
  'usage: node verify-generated.mjs [--write]'

export function parseGeneratedSemanticArguments(arguments_) {
  if (!Array.isArray(arguments_) || arguments_.some((argument) => typeof argument !== 'string')) {
    return invalidArguments()
  }
  if (arguments_.length === 0) {
    return Object.freeze({ ok: true, mode: 'verify' })
  }
  if (arguments_.length === 1 && arguments_[0] === '--write') {
    return Object.freeze({ ok: true, mode: 'write' })
  }
  return invalidArguments()
}

function invalidArguments() {
  return Object.freeze({
    ok: false,
    failure: createGeneratedSemanticFailure(
      'usage',
      'invalid-arguments',
      GENERATED_SEMANTIC_USAGE,
    ),
  })
}
