// Go treats an empty GOFLAGS as absent and falls back to persisted GOENV values.
// Whitespace is an explicit empty flag list without losing proxy or credential settings.
const NO_GO_FLAGS = ' '

export function goEnvironment(environment = process.env) {
  return { ...environment, GOTOOLCHAIN: 'local', GOWORK: 'off', GOFLAGS: NO_GO_FLAGS }
}

export function buildEnvironment(build, environment = process.env) {
  return {
    ...goEnvironment(environment),
    GOOS: build.goos,
    GOARCH: build.goarch,
    CGO_ENABLED: build.cgo_enabled ? '1' : '0',
  }
}
