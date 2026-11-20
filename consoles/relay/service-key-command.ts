import type { ServiceCredential } from './api'

const SAFE_SHELL_ARGUMENT = /^[A-Za-z0-9._~:/@-]+$/

function quoteShellArgument(value: string): string {
  if (SAFE_SHELL_ARGUMENT.test(value)) return value
  return "'" + value.replaceAll("'", "'\\''") + "'"
}

export function formatRelayAddCommand(credential: ServiceCredential): string {
  return `wind relay add ${quoteShellArgument(credential.url)} --access-key ${quoteShellArgument(credential.access_key)}`
}
