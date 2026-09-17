import type { ArtifactSpec } from '../../transfer/intent'

export const ORIGINAL_FILE_DOWNLOAD_MEDIA_TYPE = 'application/octet-stream'
const ZIP_ARCHIVE_DOWNLOAD_MEDIA_TYPE = 'application/zip'

/** Download metadata belongs to the artifact, never its opaque storage filename. */
export function artifactDownloadMediaType(artifact: ArtifactSpec): string {
  switch (artifact.kind) {
    case 'original-file': return ORIGINAL_FILE_DOWNLOAD_MEDIA_TYPE
    case 'zip-archive': return ZIP_ARCHIVE_DOWNLOAD_MEDIA_TYPE
    case 'directory-tree':
      throw new TypeError('A directory tree cannot be handed to the browser as a file')
  }
}
