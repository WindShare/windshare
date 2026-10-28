// See experiment_paths.py: the containing Syncthing folder ignores *_stignore,
// so experiment outputs use that suffix while ordinary .tmp tooling stays synced.
import path from 'node:path'

export const IGNORED_DIRECTORY_SUFFIX = '_stignore'

export function experimentName(name) {
  if (!name) throw new Error('experiment name is required')
  return name.endsWith(IGNORED_DIRECTORY_SUFFIX) ? name : name + IGNORED_DIRECTORY_SUFFIX
}

export function experimentLabel(name) {
  return name.endsWith(IGNORED_DIRECTORY_SUFFIX) ? name.slice(0, -IGNORED_DIRECTORY_SUFFIX.length) : name
}

export function experimentDirectory(root, name) {
  return path.join(root, '.tmp', experimentName(name))
}
