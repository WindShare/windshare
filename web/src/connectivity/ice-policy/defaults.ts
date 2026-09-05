import { ICEEndpointPool } from './endpoints'

export const EXISTING_DEFAULT_STUN_SERVER = 'stun:stun.l.google.com:19302'

// Preserve the shipped endpoint without inventing geography or availability.
export function defaultICEEndpointPool(): ICEEndpointPool {
 return new ICEEndpointPool([{
  id: 'shipped-google-stun', url: EXISTING_DEFAULT_STUN_SERVER,
  region: '', failureDomain: '', provider: '', family: 'any',
  trust: 'reviewed', priority: 0, enabled: true,
 }])
}
