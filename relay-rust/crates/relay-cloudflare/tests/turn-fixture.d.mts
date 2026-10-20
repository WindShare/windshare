export interface TurnFixture {
  readonly origin: string;
  provision(): Promise<{ status: string; serviceId?: string; keyId?: string }>;
  admin(path: 'credentials', body: { name: string }): Promise<{ configuration: object }>;
  dispose(): Promise<void>;
}
export function turnFixture(options?: {
  provider?: (request: Request) => Promise<Response>;
  lifetimeMs?: number;
}): Promise<TurnFixture>;
