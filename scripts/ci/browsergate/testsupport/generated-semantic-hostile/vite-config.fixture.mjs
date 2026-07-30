// The isolated worker must never discover application configuration. Throwing
// here turns accidental Vite config loading into an immediate, diagnostic failure.
throw new Error('hostile generated semantic Vite config must not be loaded')
