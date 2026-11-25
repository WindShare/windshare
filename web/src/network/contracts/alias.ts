export const validNodeAlias = (alias: string): boolean => /^[a-z](?:[a-z0-9-]{0,30}[a-z0-9])?$/.test(alias)
