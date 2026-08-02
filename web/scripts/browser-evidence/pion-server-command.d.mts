export const PION_SERVER_EXECUTABLE_ENV: 'WINDSHARE_PION_SERVER_EXECUTABLE'

export interface PionServerCommand {
  readonly executable: string
  readonly arguments: readonly []
}

export function pionServerCommand(
  environment?: Readonly<Record<string, string | undefined>>,
): Readonly<PionServerCommand>
