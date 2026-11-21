import { DurableObject } from 'cloudflare:workers'
import type { DonationsEnv } from './env'
import type { PublicWallResponse } from './model'

const EMPTY_WALL: PublicWallResponse = Object.freeze({
  version: 0,
  moderationEnabled: false,
  entries: [],
})

export class DonationWall extends DurableObject<DonationsEnv> {
  async fetch(request: Request): Promise<Response> {
    const url = new URL(request.url)
    if (request.method === 'GET' && url.pathname === '/internal/wall') {
      return Response.json(EMPTY_WALL, {
        headers: {
          'ETag': '"wall-v0"',
        },
      })
    }
    return Response.json({ error: 'not_found' }, { status: 404 })
  }
}
