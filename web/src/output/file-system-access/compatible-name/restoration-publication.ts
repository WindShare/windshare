import { provenRestorationTemplateProvider, type RestorationTemplate, type RestorationTemplateProvider } from './restoration-template'

/** Name mappings and their report remain usable without a script for the current host. */
export function selectRestorationTemplate(options: {
  readonly platform: string
  readonly templateProvider?: RestorationTemplateProvider
}): RestorationTemplate | undefined {
  try { return (options.templateProvider ?? provenRestorationTemplateProvider).select(options.platform) }
  catch { return undefined }
}

/** Script publication cannot acquire authority over the mapped files or their durable ledger. */
export async function publishRestorationScript(
  template: RestorationTemplate | undefined,
  publish: (source: string) => Promise<void>,
): Promise<boolean> {
  if (template === undefined) return false
  try { await publish(template.source); return true }
  catch { return false }
}
