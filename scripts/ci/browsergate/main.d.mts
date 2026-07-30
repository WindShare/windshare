export interface ContainmentValidationOptions {
  readonly platform?: NodeJS.Platform
  readonly createImageAuthority?: () => unknown
  readonly createLifecycle?: (
    options: Readonly<Record<string, unknown>>,
  ) => unknown
  readonly executeRegression?: (
    operationId: string,
    command: Readonly<Record<string, unknown>>,
  ) => unknown
}

export function runContainmentValidation(
  options?: ContainmentValidationOptions,
): Promise<0>
