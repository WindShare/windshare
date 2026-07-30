const INHERITED_SAMPLE_ENVIRONMENT_NAMES = Object.freeze([
  'APPDATA',
  'COMSPEC',
  'DBUS_SESSION_BUS_ADDRESS',
  'DISPLAY',
  'HOME',
  'LANG',
  'LC_ALL',
  'LD_LIBRARY_PATH',
  'LOCALAPPDATA',
  'LOGNAME',
  'PATH',
  'PATHEXT',
  'PLAYWRIGHT_BROWSERS_PATH',
  'PROGRAMDATA',
  'SSL_CERT_DIR',
  'SSL_CERT_FILE',
  'SYSTEMROOT',
  'TEMP',
  'TMP',
  'TZ',
  'USER',
  'USERPROFILE',
  'WAYLAND_DISPLAY',
  'WINDIR',
  'XDG_CONFIG_HOME',
  'XDG_RUNTIME_DIR',
] as const)

const FORBIDDEN_SAMPLE_ENVIRONMENT_NAMES = new Set([
  'ACTIONS_ID_TOKEN_REQUEST_TOKEN',
  'ACTIONS_RUNTIME_TOKEN',
  'GH_TOKEN',
  'GITHUB_TOKEN',
  'NODE_AUTH_TOKEN',
  'NPM_TOKEN',
])

export function inheritedSampleEnvironment(
  source: Readonly<Record<string, string | undefined>> = process.env,
  platform: NodeJS.Platform = process.platform,
): Readonly<Record<string, string>> {
  const byCanonicalName = new Map(Object.entries(source).map(([name, value]) => [
    platform === 'win32' ? name.toUpperCase() : name,
    value,
  ]))
  const selected: Record<string, string> = {}
  for (const canonicalName of INHERITED_SAMPLE_ENVIRONMENT_NAMES) {
    const value = byCanonicalName.get(canonicalName)
    // Environment names are case-insensitive on Windows. Preserving the
    // spelling supplied by an intermediate PowerShell process made equivalent
    // launch environments hash differently across the D5 trust handoff.
    if (value !== undefined) selected[canonicalName] = value
  }
  return Object.freeze(selected)
}

export function sampleProcessEnvironment(
  commandEnvironment: Readonly<Record<string, string>> | undefined,
  injectedEnvironment: Readonly<Record<string, string>>,
  inherited: Readonly<Record<string, string | undefined>> = process.env,
  platform: NodeJS.Platform = process.platform,
): Readonly<Record<string, string>> {
  const explicit = {
    ...(commandEnvironment ?? {}),
    ...injectedEnvironment,
  }
  for (const [name, value] of Object.entries(explicit)) {
    if (!/^[A-Za-z_]\w*$/u.test(name)) {
      throw new Error(`sample environment name ${JSON.stringify(name)} is invalid`)
    }
    if (FORBIDDEN_SAMPLE_ENVIRONMENT_NAMES.has(name.toUpperCase())) {
      throw new Error(`sample environment ${name} is a forbidden credential channel`)
    }
    if (typeof value !== 'string') {
      throw new Error(`sample environment ${name} must be text`)
    }
  }
  return Object.freeze({
    ...inheritedSampleEnvironment(inherited, platform),
    ...explicit,
  })
}
