import { createHash } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

export const STABILITY_EXECUTION_CONTRACT_SCHEMA_VERSION =
  'windshare.stability-execution-contract/v3'

export const STABILITY_WORKFLOW_PATH = '.github/workflows/stability.yml'

const MAXIMUM_SOURCE_BYTES = 1_048_576
const GIT_BLOB_SHA1_PATTERN = /^[a-f0-9]{40}$/u
const SHA256_PATTERN = /^[a-f0-9]{64}$/u
const SOURCE_ROLES = Object.freeze({
  workflow: 'workflow',
  integration: 'integration-entrypoint',
  goAuthority: 'go-executable-authority',
  runIdentity: 'test-run-identity-authority',
  evidenceRunner: 'evidence-runner',
})
const STABILITY_HANDSHAKE_VARIABLES = Object.freeze([
  'WINDSHARE_STABILITY_START_REQUEST',
  'WINDSHARE_STABILITY_STARTED_OUTPUT',
  'WINDSHARE_STABILITY_START_SECRET',
])
const EXECUTION_AUTHORITIES = Object.freeze({
  linux: Object.freeze({
    workflowCommand: 'bash scripts/ci/linux/integration.sh',
    integrationCommand: 'windshare_go_test_json -count=1 ./integration/...',
    sourceDefinitions: Object.freeze([
      Object.freeze({ role: SOURCE_ROLES.workflow, path: STABILITY_WORKFLOW_PATH }),
      Object.freeze({ role: SOURCE_ROLES.evidenceRunner, path: 'scripts/ci/stability/result.mjs' }),
      Object.freeze({ role: SOURCE_ROLES.integration, path: 'scripts/ci/linux/integration.sh' }),
      Object.freeze({ role: SOURCE_ROLES.goAuthority, path: 'scripts/ci/goauthority/authority.sh' }),
      Object.freeze({ role: SOURCE_ROLES.runIdentity, path: 'scripts/ci/test-run-id.sh' }),
    ]),
  }),
  windows: Object.freeze({
    workflowCommand: './scripts/ci/windows/integration.ps1',
    integrationCommand: 'Invoke-WindShareGoTestJSON -count=1 ./integration/...',
    sourceDefinitions: Object.freeze([
      Object.freeze({ role: SOURCE_ROLES.workflow, path: STABILITY_WORKFLOW_PATH }),
      Object.freeze({ role: SOURCE_ROLES.evidenceRunner, path: 'scripts/ci/stability/result.mjs' }),
      Object.freeze({ role: SOURCE_ROLES.integration, path: 'scripts/ci/windows/integration.ps1' }),
      Object.freeze({ role: SOURCE_ROLES.goAuthority, path: 'scripts/ci/goauthority/authority.psm1' }),
      Object.freeze({ role: SOURCE_ROLES.runIdentity, path: 'scripts/ci/test-run-id.psm1' }),
    ]),
  }),
})

export function loadCurrentStabilityExecutionSources({ operatingSystem, repositoryRoot }) {
  const authority = requireOperatingSystem(operatingSystem)
  const root = resolve(repositoryRoot)
  return Object.freeze(authority.sourceDefinitions.map(({ role, path }) => {
    const source = readFileSync(resolve(root, path))
    return Object.freeze({ role, path, source, gitBlobSha1: gitBlobSha1(source) })
  }))
}

export function loadCurrentStabilityExecutionContract({ operatingSystem, repositoryRoot }) {
  return createStabilityExecutionContract({
    operatingSystem,
    sources: loadCurrentStabilityExecutionSources({ operatingSystem, repositoryRoot }),
  })
}

export function createStabilityExecutionContract({ operatingSystem, sources }) {
  const authority = requireOperatingSystem(operatingSystem)
  const verifiedSources = requireExecutionSources(sources, authority)
  const sourceText = new Map(verifiedSources.map((source) => [
    source.role,
    decodeSource(source.bytes, `${source.role} source`),
  ]))

  validateWorkflowAuthority(sourceText.get(SOURCE_ROLES.workflow))
  validateEvidenceRunnerAuthority(sourceText.get(SOURCE_ROLES.evidenceRunner))
  validateIntegrationAuthority(sourceText.get(SOURCE_ROLES.integration), operatingSystem, authority)
  validateGoAuthority(sourceText.get(SOURCE_ROLES.goAuthority), operatingSystem)
  validateRunIdentityAuthority(sourceText.get(SOURCE_ROLES.runIdentity), operatingSystem)

  const payload = Object.freeze({
    schema_version: STABILITY_EXECUTION_CONTRACT_SCHEMA_VERSION,
    operating_system: operatingSystem,
    workflow_command: authority.workflowCommand,
    integration_command: authority.integrationCommand,
    invocation_count: 1,
    retry_policy: 'forbidden',
    sources: Object.freeze(verifiedSources.map(({ role, path, gitBlobSha1: blob, contentSha256 }) =>
      Object.freeze({
        role,
        path,
        git_blob_sha1: blob,
        content_sha256: contentSha256,
        semantic_sha256: semanticSourceSha256(sourceText.get(role), operatingSystem, role),
      }))),
  })
  const semanticPayload = semanticContractPayload(payload)
  return Object.freeze({
    ...payload,
    semantic_contract_sha256: sha256(Buffer.from(JSON.stringify(semanticPayload), 'utf8')),
    contract_sha256: sha256(Buffer.from(JSON.stringify(payload), 'utf8')),
  })
}

export function parseStabilityExecutionContract(value) {
  const record = exactRecord(value, [
    'schema_version',
    'operating_system',
    'workflow_command',
    'integration_command',
    'invocation_count',
    'retry_policy',
    'sources',
    'semantic_contract_sha256',
    'contract_sha256',
  ])
  const authority = requireOperatingSystem(record.operating_system)
  if (
    record.schema_version !== STABILITY_EXECUTION_CONTRACT_SCHEMA_VERSION ||
    record.workflow_command !== authority.workflowCommand ||
    record.integration_command !== authority.integrationCommand ||
    record.invocation_count !== 1 || record.retry_policy !== 'forbidden' ||
    typeof record.semantic_contract_sha256 !== 'string' ||
    !SHA256_PATTERN.test(record.semantic_contract_sha256) ||
    typeof record.contract_sha256 !== 'string' || !SHA256_PATTERN.test(record.contract_sha256)
  ) {
    throw new Error('stability execution contract is invalid')
  }
  if (!Array.isArray(record.sources) || record.sources.length !== authority.sourceDefinitions.length) {
    throw new Error('stability execution contract source closure is invalid')
  }
  const parsedSources = Object.freeze(record.sources.map((value, index) => {
    const source = exactRecord(value, [
      'role',
      'path',
      'git_blob_sha1',
      'content_sha256',
      'semantic_sha256',
    ])
    const expected = authority.sourceDefinitions[index]
    if (
      source.role !== expected.role || source.path !== expected.path ||
      typeof source.git_blob_sha1 !== 'string' || !GIT_BLOB_SHA1_PATTERN.test(source.git_blob_sha1) ||
      typeof source.content_sha256 !== 'string' || !SHA256_PATTERN.test(source.content_sha256) ||
      typeof source.semantic_sha256 !== 'string' || !SHA256_PATTERN.test(source.semantic_sha256)
    ) {
      throw new Error('stability execution contract source descriptor is invalid')
    }
    return Object.freeze({
      role: expected.role,
      path: expected.path,
      git_blob_sha1: source.git_blob_sha1,
      content_sha256: source.content_sha256,
      semantic_sha256: source.semantic_sha256,
    })
  }))
  const parsed = Object.freeze({
    schema_version: STABILITY_EXECUTION_CONTRACT_SCHEMA_VERSION,
    operating_system: record.operating_system,
    workflow_command: authority.workflowCommand,
    integration_command: authority.integrationCommand,
    invocation_count: 1,
    retry_policy: 'forbidden',
    sources: parsedSources,
    semantic_contract_sha256: record.semantic_contract_sha256,
    contract_sha256: record.contract_sha256,
  })
  const { contract_sha256: digest, semantic_contract_sha256: semanticDigest, ...payload } = parsed
  if (sha256(Buffer.from(JSON.stringify(payload), 'utf8')) !== digest) {
    throw new Error('stability execution contract digest is invalid')
  }
  if (sha256(Buffer.from(JSON.stringify(semanticContractPayload(payload)), 'utf8')) !== semanticDigest) {
    throw new Error('stability execution contract semantic digest is invalid')
  }
  return parsed
}

export function executionContractsEqual(left, right) {
  const leftContract = parseStabilityExecutionContract(left)
  const rightContract = parseStabilityExecutionContract(right)
  return leftContract.semantic_contract_sha256 === rightContract.semantic_contract_sha256
}

export function executionContractEvidenceEqual(left, right) {
  return JSON.stringify(parseStabilityExecutionContract(left)) ===
    JSON.stringify(parseStabilityExecutionContract(right))
}

function semanticContractPayload(contract) {
  return Object.freeze({
    schema_version: contract.schema_version,
    operating_system: contract.operating_system,
    workflow_command: contract.workflow_command,
    integration_command: contract.integration_command,
    invocation_count: contract.invocation_count,
    retry_policy: contract.retry_policy,
    sources: Object.freeze(contract.sources.map(({ role, path, semantic_sha256: semanticSha256 }) =>
      Object.freeze({ role, path, semantic_sha256: semanticSha256 }))),
  })
}

function semanticSourceSha256(source, operatingSystem, role) {
  const semanticValue = role === SOURCE_ROLES.workflow
    ? parseWorkflowSemanticDocument(source)
    : parseHelperSemanticManifest(source, operatingSystem, role)
  return sha256(Buffer.from(JSON.stringify(semanticValue), 'utf8'))
}

function parseHelperSemanticManifest(source, operatingSystem, role) {
  const expression = role === SOURCE_ROLES.evidenceRunner
    ? /^\s*(?:export\s+)?const\s+STABILITY_RESULT_RUNNER_SEMANTICS\s*=\s*'([^'\r\n]+)'\s*$/gmu
    : operatingSystem === 'linux'
      ? /^\s*readonly\s+WINDSHARE_STABILITY_[A-Z_]+_SEMANTICS\s*=\s*'([^'\r\n]+)'\s*$/gmu
      : /^\s*\$script:WindShareStabilityHelperSemantics\s*=\s*'([^'\r\n]+)'\s*$/gimu
  const matches = [...source.matchAll(expression)]
  if (matches.length !== 1) {
    throw new Error(`${operatingSystem} ${role} must declare one stability semantic manifest`)
  }
  let manifest
  try {
    manifest = JSON.parse(matches[0][1])
  } catch (cause) {
    throw new Error(`${operatingSystem} ${role} stability semantic manifest is invalid JSON`, { cause })
  }
  const record = exactRecord(manifest, [
    'schema_version',
    'operating_system',
    'role',
    'revision',
    'command_plan',
  ])
  if (
    record.schema_version !== 'windshare.stability-helper-semantics/v1' ||
    record.operating_system !== (role === SOURCE_ROLES.evidenceRunner ? 'cross-platform' : operatingSystem) ||
    record.role !== role ||
    !Number.isSafeInteger(record.revision) || record.revision < 1 ||
    !Array.isArray(record.command_plan) || record.command_plan.length === 0 ||
    record.command_plan.some((step) => typeof step !== 'string' || !/^[a-z0-9]+(?:-[a-z0-9]+)*$/u.test(step))
  ) {
    throw new Error(`${operatingSystem} ${role} stability semantic manifest is invalid`)
  }
  return Object.freeze({
    schema_version: record.schema_version,
    operating_system: record.operating_system,
    role: record.role,
    revision: record.revision,
    command_plan: Object.freeze([...record.command_plan]),
  })
}

function parseWorkflowSemanticDocument(source) {
  const lines = source.split(/\r?\n/u)
  const records = []
  let block
  for (let index = 0; index < lines.length; index += 1) {
    const rawLine = lines[index]
    const indentation = leadingSpaces(rawLine)
    if (block !== undefined && (rawLine.trim() === '' || indentation > block.parentIndentation)) {
      if (rawLine.trim() === '') {
        records.push(Object.freeze({ kind: 'block-line', indentation: 0, value: '' }))
        continue
      }
      block.contentIndentation ??= indentation
      if (indentation < block.contentIndentation) {
        throw new Error('stability workflow block scalar indentation is inconsistent')
      }
      records.push(Object.freeze({
        kind: 'block-line',
        indentation: indentation - block.contentIndentation,
        value: rawLine.slice(block.contentIndentation).replace(/[ \t]+$/u, ''),
      }))
      continue
    }
    block = undefined
    if (rawLine.includes('\t')) throw new Error('stability workflow must not use tab indentation')
    const withoutComment = removeYamlComment(rawLine)
    if (withoutComment.trim() === '') continue
    if (indentation % 2 !== 0) throw new Error('stability workflow indentation must use two-space levels')
    const value = normalizeYamlLine(withoutComment.trim())
    records.push(Object.freeze({ kind: 'yaml-line', indentation: indentation / 2, value }))
    if (/:[|>][+-]?$/u.test(value)) block = { parentIndentation: indentation, contentIndentation: undefined }
  }
  if (records.length === 0) throw new Error('stability workflow semantic document is empty')
  return Object.freeze(records)
}

function removeYamlComment(line) {
  let quote
  let escaped = false
  for (let index = 0; index < line.length; index += 1) {
    const character = line[index]
    if (quote === '"') {
      if (escaped) escaped = false
      else if (character === '\\') escaped = true
      else if (character === quote) quote = undefined
      continue
    }
    if (quote === "'") {
      if (character === quote && line[index + 1] === quote) index += 1
      else if (character === quote) quote = undefined
      continue
    }
    if (character === '"' || character === "'") {
      quote = character
      continue
    }
    if (character === '#' && (index === 0 || /\s/u.test(line[index - 1]))) {
      return line.slice(0, index).replace(/[ \t]+$/u, '')
    }
  }
  if (quote !== undefined) throw new Error('stability workflow contains an unterminated quoted scalar')
  return line.replace(/[ \t]+$/u, '')
}

function normalizeYamlLine(line) {
  let normalized = ''
  let quote
  let escaped = false
  let pendingSpace = false
  for (let index = 0; index < line.length; index += 1) {
    const character = line[index]
    if (quote !== undefined) {
      normalized += character
      if (quote === '"') {
        if (escaped) escaped = false
        else if (character === '\\') escaped = true
        else if (character === quote) quote = undefined
      } else if (character === quote && line[index + 1] === quote) {
        normalized += line[index + 1]
        index += 1
      } else if (character === quote) {
        quote = undefined
      }
      continue
    }
    if (/\s/u.test(character)) {
      pendingSpace = normalized.length > 0
      continue
    }
    if (pendingSpace && !/[:,\]}]/u.test(character) && !/[{[,:]/u.test(normalized.at(-1))) {
      normalized += ' '
    }
    pendingSpace = false
    normalized += character
    if (character === '"' || character === "'") quote = character
  }
  return normalized
}

function leadingSpaces(value) {
  let count = 0
  while (value[count] === ' ') count += 1
  return count
}

function requireExecutionSources(value, authority) {
  if (!Array.isArray(value) || value.length !== authority.sourceDefinitions.length) {
    throw new Error('stability execution source closure is incomplete')
  }
  return Object.freeze(value.map((candidate, index) => {
    if (candidate === null || typeof candidate !== 'object' || Array.isArray(candidate)) {
      throw new Error('stability execution source must be an object')
    }
    const expected = authority.sourceDefinitions[index]
    if (candidate.role !== expected.role || candidate.path !== expected.path) {
      throw new Error('stability execution source role or path is not canonical')
    }
    const bytes = requireSourceBytes(candidate.source, `${expected.role} source`)
    const computedGitBlobSha1 = gitBlobSha1(bytes)
    if (candidate.gitBlobSha1 !== undefined && candidate.gitBlobSha1 !== computedGitBlobSha1) {
      throw new Error(`${expected.role} source Git blob identity disagrees with its content`)
    }
    return Object.freeze({
      role: expected.role,
      path: expected.path,
      bytes,
      gitBlobSha1: computedGitBlobSha1,
      contentSha256: sha256(bytes),
    })
  }))
}

function validateWorkflowAuthority(source) {
  const runnerCommand = 'node scripts/ci/stability/result.mjs run'
  if (source.split(runnerCommand).length - 1 !== Object.keys(EXECUTION_AUTHORITIES).length) {
    throw new Error('stability workflow must invoke one evidence runner per operating system')
  }
  for (const { workflowCommand } of Object.values(EXECUTION_AUTHORITIES)) {
    requireExactLine(
      source,
      `--entrypoint "${workflowCommand}"`,
      `stability workflow must bind ${workflowCommand} to one evidence runner`,
    )
    if (source.split(workflowCommand).length - 1 !== 1) {
      throw new Error(`stability workflow contains an ambiguous ${workflowCommand} authority`)
    }
  }
}

function validateEvidenceRunnerAuthority(source) {
  const requiredFragments = [
    'const STABILITY_RESULT_RUNNER_SEMANTICS =',
    'writeCanonicalJSON(outputPath, event)',
    'const product = spawnCanonicalIntegration(operatingSystem, {',
    'if (!existsSync(startedOutput)) {',
    'writeCanonicalJSON(output, result)',
    'return Number.isSafeInteger(product.status) ? product.status : 1',
  ]
  for (const fragment of requiredFragments) {
    if (!source.includes(fragment)) {
      throw new Error('stability evidence runner is missing lifecycle or exit-code authority')
    }
  }
  if (/\b(?:for|while)\s*\([^)]*(?:attempt|retry)/iu.test(source)) {
    throw new Error('stability evidence runner must not contain an integration retry construct')
  }
}

function validateIntegrationAuthority(source, operatingSystem, authority) {
  requireExactLine(
    source,
    authority.integrationCommand,
    `${operatingSystem} stability integration must invoke the canonical Go command exactly once`,
  )
  if (source.split(authority.integrationCommand).length - 1 !== 1) {
    throw new Error(`${operatingSystem} stability integration contains an ambiguous Go command authority`)
  }
  const retryConstruct = operatingSystem === 'linux'
    ? /^\s*(?:for|while|until)\b|\bretry\b|\bseq\b/mu
    : /^\s*(?:foreach|for|while|do)\b|\bretry\b/imu
  if (retryConstruct.test(source)) {
    throw new Error(`${operatingSystem} stability integration must not contain an internal retry construct`)
  }

  validateStabilityHandshakeMode(source, operatingSystem, authority)

  const orderedMilestones = operatingSystem === 'linux'
    ? [
        'readonly stability_evidence_mode',
        'source scripts/ci/goauthority/authority.sh',
        'windshare_enter_go_authority',
        'source scripts/ci/test-run-id.sh',
        'generated_run_id="$(new_windshare_test_run_id integration)"',
        'export WINDSHARE_TEST_RUN_ID="$generated_run_id"',
        'node scripts/ci/stability/result.mjs started',
        authority.integrationCommand,
      ]
    : [
        "$stabilityEvidenceMode = 'ordinary'",
        "Import-Module (Join-Path $ciRoot 'goauthority/authority.psm1') -Force",
        '$null = Enter-WindShareGoAuthority',
        "Import-Module (Join-Path $ciRoot 'test-run-id.psm1') -Force",
        "Invoke-WithWindShareTestRunID -Suite 'integration' -Body {",
        'node scripts/ci/stability/result.mjs started',
        authority.integrationCommand,
      ]
  let previousOffset = -1
  for (const milestone of orderedMilestones) {
    requireExactLine(source, milestone, `${operatingSystem} stability integration authority milestone is missing`)
    const offset = source.indexOf(milestone)
    if (offset <= previousOffset) {
      throw new Error(`${operatingSystem} stability integration authority is ordered incorrectly`)
    }
    previousOffset = offset
  }
  rejectGoAlias(source, operatingSystem, 'integration entrypoint')
  const bareGo = operatingSystem === 'linux'
    ? /^\s*(?:command\s+)?go(?:\s|$)/mu
    : /^\s*(?:&\s*)?go(?:\.exe)?(?:\s|$)/imu
  if (bareGo.test(source)) {
    throw new Error(`${operatingSystem} stability integration bypasses retained Go authority`)
  }
}

function validateStabilityHandshakeMode(source, operatingSystem, authority) {
  const startCommand = 'node scripts/ci/stability/result.mjs started'
  if (source.split(startCommand).length - 1 !== 1) {
    throw new Error(`${operatingSystem} stability integration must contain one start handshake`)
  }

  for (const name of STABILITY_HANDSHAKE_VARIABLES) {
    const presenceFragment = operatingSystem === 'linux'
      ? `[[ -v ${name} ]]`
      : `.Contains('${name}')`
    if (!source.includes(presenceFragment)) {
      throw new Error(
        `${operatingSystem} stability integration must classify every handshake variable by presence`,
      )
    }
  }

  const cardinalityFragments = operatingSystem === 'linux'
    ? [
        `readonly stability_handshake_variable_count=${STABILITY_HANDSHAKE_VARIABLES.length}`,
        'stability_handshake_presence_count != 0',
        'stability_handshake_presence_count != stability_handshake_variable_count',
        'stability_handshake_presence_count == stability_handshake_variable_count',
      ]
    : [
        `$stabilityHandshakeVariableCount = ${STABILITY_HANDSHAKE_VARIABLES.length}`,
        '$stabilityHandshakePresenceCount -ne 0',
        '$stabilityHandshakePresenceCount -ne $stabilityHandshakeVariableCount',
        '$stabilityHandshakePresenceCount -eq $stabilityHandshakeVariableCount',
      ]
  for (const fragment of cardinalityFragments) {
    if (!source.includes(fragment)) {
      throw new Error(
        `${operatingSystem} stability integration must distinguish all-present from all-absent handshake state`,
      )
    }
  }

  const guardedStart = operatingSystem === 'linux'
    ? [
        'if [[ "$stability_evidence_mode" == authenticated ]]; then',
        `  ${startCommand}`,
        `  unset ${STABILITY_HANDSHAKE_VARIABLES.join(' ')}`,
        'fi',
        authority.integrationCommand,
      ].join('\n')
    : [
        "    if ($stabilityEvidenceMode -eq 'authenticated') {",
        `        ${startCommand}`,
        '        if ($LASTEXITCODE -ne 0) {',
        '            throw "stability start handshake exited with code $LASTEXITCODE"',
        '        }',
        `        Remove-Item Env:${STABILITY_HANDSHAKE_VARIABLES.join(', Env:')} -ErrorAction SilentlyContinue`,
        '    }',
        `    ${authority.integrationCommand}`,
      ].join('\n')
  if (!source.includes(guardedStart)) {
    throw new Error(
      `${operatingSystem} stability integration must publish its start only in authenticated mode immediately before Go`,
    )
  }
}

function validateGoAuthority(source, operatingSystem) {
  rejectGoAlias(source, operatingSystem, 'Go authority helper')
  const requiredFragments = operatingSystem === 'linux'
    ? [
        'windshare_enter_go_authority() {',
        'exec {WINDSHARE_GO_DESCRIPTOR}<"$candidate"',
        'retained_executable="/proc/$BASHPID/fd/$WINDSHARE_GO_DESCRIPTOR"',
        'GOENV=off GOTOOLCHAIN=local "$retained_executable"',
        'windshare_assert_go_authority_active() {',
        'windshare_go() {',
        '"$WINDSHARE_GO_EXECUTABLE" "$@"',
        'windshare_go_test_json() {',
        'windshare_go test -json "$@"',
      ]
    : [
        'function Enter-WindShareGoAuthority {',
        '$retainedStream = [IO.FileStream]::new(',
        '[IO.FileShare]::Read',
        "$env:GOENV = 'off'",
        "$env:GOTOOLCHAIN = 'local'",
        'function Assert-WindShareGoAuthorityActive {',
        'function Invoke-WindShareGo {',
        '& $script:WindShareGoAuthority.Executable @goArguments',
        'function Invoke-WindShareGoTestJSON {',
        'Invoke-WindShareGo test -json @testArguments',
      ]
  for (const fragment of requiredFragments) {
    if (!source.includes(fragment)) {
      throw new Error(`${operatingSystem} Go authority helper is missing retained-executable semantics`)
    }
  }
}

function validateRunIdentityAuthority(source, operatingSystem) {
  rejectGoAlias(source, operatingSystem, 'test-run identity helper')
  const assertion = operatingSystem === 'linux'
    ? 'windshare_assert_go_authority_active || return 1'
    : 'Assert-WindShareGoAuthorityActive'
  if (!source.includes(assertion)) {
    throw new Error(`${operatingSystem} test-run identity helper does not require settled Go authority`)
  }
}

function rejectGoAlias(source, operatingSystem, label) {
  const pattern = operatingSystem === 'linux'
    ? /(?:^|\n)\s*(?:(?:function\s+)?go\s*\(\s*\)|alias\s+go\s*=)/u
    : /(?:^|\n)\s*(?:function\s+(?:global:)?go\b|(?:New|Set)-Alias\s+(?:-Name\s+)?go\b)/iu
  if (pattern.test(source)) throw new Error(`${operatingSystem} ${label} must not redefine Go`)
}

function requireExactLine(source, expected, message) {
  const matches = source.split(/\r?\n/u).filter((line) => line.trim() === expected)
  if (matches.length !== 1) throw new Error(message)
}

function requireOperatingSystem(value) {
  const authority = EXECUTION_AUTHORITIES[value]
  if (authority === undefined) throw new Error('stability execution contract operating system is invalid')
  return authority
}

function requireSourceBytes(value, label) {
  if (typeof value !== 'string' && !Buffer.isBuffer(value) && !(value instanceof Uint8Array)) {
    throw new Error(`${label} must be bytes or text`)
  }
  const bytes = typeof value === 'string'
    ? Buffer.from(value, 'utf8')
    : Buffer.from(value.buffer, value.byteOffset, value.byteLength)
  if (bytes.byteLength === 0 || bytes.byteLength > MAXIMUM_SOURCE_BYTES) {
    throw new Error(`${label} source size is invalid`)
  }
  return bytes
}

function decodeSource(bytes, label) {
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch (cause) {
    throw new Error(`${label} source is not UTF-8`, { cause })
  }
}

function gitBlobSha1(bytes) {
  const header = Buffer.from(`blob ${bytes.byteLength}\0`, 'utf8')
  return createHash('sha1').update(header).update(bytes).digest('hex')
}

function sha256(bytes) {
  return createHash('sha256').update(bytes).digest('hex')
}

function exactRecord(value, keys) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error('stability execution contract must be an object')
  }
  const actual = Object.keys(value)
  if (actual.length !== keys.length || actual.some((key, index) => key !== keys[index])) {
    throw new Error('stability execution contract fields are not canonical')
  }
  return value
}
