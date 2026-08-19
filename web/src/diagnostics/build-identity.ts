import type {} from './build-identity-env'
import type { BuildIdentityV1 } from './export/incident-record-v1'
import type { BuildSnapshot } from './export/projector'

const BUILD_REVISION_PATTERN = /^[0-9a-f]{7,64}$/

export function browserBuildSnapshot(): BuildSnapshot {
  return Object.freeze({
    version: __WIND_BUILD_VERSION__,
    ...(__WIND_BUILD_REVISION__ === undefined ||
        !BUILD_REVISION_PATTERN.test(__WIND_BUILD_REVISION__)
      ? {}
      : { revision: __WIND_BUILD_REVISION__ }),
    mode: __WIND_BUILD_MODE__,
  })
}

export function browserBuildIdentity(
  snapshot: BuildSnapshot = browserBuildSnapshot(),
): BuildIdentityV1 {
  return Object.freeze({
    application: 'windshare_web',
    version: snapshot.version,
    ...(snapshot.revision === undefined ? {} : { revision: snapshot.revision }),
    mode: snapshot.mode,
  })
}
