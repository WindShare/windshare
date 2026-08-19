export {}

declare global {
  const __WIND_BUILD_VERSION__: string
  const __WIND_BUILD_REVISION__: string | undefined
  const __WIND_BUILD_MODE__: 'development' | 'production' | 'test'
}
