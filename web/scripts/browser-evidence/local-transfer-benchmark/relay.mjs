import { mkdir, writeFile } from 'node:fs/promises'
import { join, resolve } from 'node:path'

const ephemeralLoopback = '127.0.0.1:0'

export async function localRelayArguments(output, origin) {
  const state = resolve(output, 'relay-state')
  const config = join(state, 'node.json')
  await mkdir(state)
  // Relative storage paths keep each run's identity and durable state beside its configuration.
  await writeFile(config, JSON.stringify({
    listen: ephemeralLoopback,
    database: 'relay.sqlite',
    identity_file: 'identity.json',
    origins: { kind: 'allowlist', origins: [origin], allow_localhost: false },
    stun: { udp_addresses: [ephemeralLoopback], admin_address: ephemeralLoopback },
    node: { services: { relay: true, stun: true }, access: { anonymous: true } },
  }, null, 2) + '\n', { flag: 'wx' })
  return ['--config', config]
}
