import { inspectFileSystemComponent, PathComponentRejectedError } from '../../src/output/browser/filesystem-component-inspection'
import { prepareCompatibleNameRootRepair } from '../../src/output/file-system-access/compatible-name/coordinator'
import { appendProtectedSuffix } from '../../src/transfer/intent/selection'
import { encodeBase64Url } from '../../src/crypto/bytes'

export async function exerciseNativeNameBudget(parent: FileSystemDirectoryHandle, rawName: string) {
  const samples = [
    { kind: 'file' as const, name: rawName.normalize('NFC') },
    { kind: 'directory' as const, name: appendProtectedSuffix('a'.repeat(250), '-selection') },
  ]
  const results = []
  for (const sample of samples) {
    let refusal: PathComponentRejectedError | undefined
    try {
      await inspectFileSystemComponent({
        verifiedParent: parent, component: sample.name, expectedKind: sample.kind,
        stage: 'fsa.root.entry.inspect', mode: 'classify-rejection',
      })
    } catch (error) {
      if (!(error instanceof PathComponentRejectedError)) throw error
      refusal = error
    }
    if (refusal === undefined) throw new Error('Overlong root was admitted without repair')
    const prepared = await prepareCompatibleNameRootRepair({
      rejection: refusal, parent, entryKind: sample.kind, logicalReservedName: sample.name,
      operationId: encodeBase64Url(new Uint8Array(16).fill(1)),
      authorityRef: encodeBase64Url(new Uint8Array(32).fill(2)),
    }, {
      platform: 'linux', templateProvider: { select: () => { throw new Error('No script template') } },
      randomBits: () => 0,
    })
    const physicalName = prepared.bootstrap.initialMapping.physicalComponent
    if (sample.kind === 'directory') {
      const directory = await parent.getDirectoryHandle(physicalName, { create: true })
      const child = await directory.getFileHandle('received.txt', { create: true })
      const writer = await child.createWritable()
      await writer.write('directory received')
      await writer.close()
    } else {
      const file = await parent.getFileHandle(physicalName, { create: true })
      const writer = await file.createWritable()
      await writer.write('file received')
      await writer.close()
    }
    results.push({
      kind: sample.kind, logicalName: sample.name, physicalName,
      evidenceKind: refusal.evidenceKind, mappingName: prepared.bootstrap.initialMapping.logicalPath[0],
    })
  }
  return results
}
