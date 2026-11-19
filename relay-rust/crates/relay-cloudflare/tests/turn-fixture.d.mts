export interface TurnFixture {
  readonly origin: string;
  provision(): Promise<{ status: string; serviceId?: string; keyId?: string }>;
  admin(path: 'credentials', body: { name: string }): Promise<{
    configuration: { readonly url: string; readonly access_key: string };
  }>;
  dispose(): Promise<void>;
}
export function turnFixture(options?: {
  provider?: (request: Request) => Promise<Response>;
  lifetimeMs?: number;
}): Promise<TurnFixture>;
