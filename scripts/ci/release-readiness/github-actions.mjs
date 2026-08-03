import {
  encodedRepositoryPath,
  requireCanonicalTimestamp,
  requirePositiveSafeInteger,
  requireRepository,
  requireToken,
} from './contract.mjs'

export const GITHUB_ACTIONS_API_VERSION = '2026-03-10'
export const PAGE_SIZE = 100
export const MAXIMUM_COLLECTION_ITEMS = 1000
export const MAXIMUM_LIST_PAGES = 10
export const REQUEST_TIMEOUT_MILLISECONDS = 15_000

const GITHUB_API_ORIGIN = 'https://api.github.com'
const USER_AGENT = 'windshare-release-readiness-resolver'

export class GitHubActionsAPIError extends Error {
  constructor(code, message, { path, status, cause } = {}) {
    super(message, { cause })
    this.name = 'GitHubActionsAPIError'
    this.code = code
    if (path !== undefined) this.path = path
    if (status !== undefined) this.status = status
  }
}

export function createGitHubActionsClient({
  repository,
  token,
  fetchImpl = globalThis.fetch,
  requestTimeoutMilliseconds = REQUEST_TIMEOUT_MILLISECONDS,
}) {
  const canonicalRepository = requireRepository(repository)
  const repositoryPath = encodedRepositoryPath(canonicalRepository)
  const canonicalToken = requireToken(token)
  if (typeof fetchImpl !== 'function') {
    throw new GitHubActionsAPIError(
      'invalid-fetch-implementation',
      'GitHub API fetch implementation is missing',
    )
  }
  if (
    !Number.isSafeInteger(requestTimeoutMilliseconds) ||
    requestTimeoutMilliseconds < 1 ||
    requestTimeoutMilliseconds > REQUEST_TIMEOUT_MILLISECONDS
  ) {
    throw new GitHubActionsAPIError(
      'invalid-request-timeout',
      'GitHub API request timeout is outside its safety bound',
    )
  }

  const getJSON = (path) =>
    requestJSON({
      fetchImpl,
      token: canonicalToken,
      path,
      requestTimeoutMilliseconds,
    })

  return Object.freeze({
    async getRepository() {
      return getJSON(`/repos/${repositoryPath}`)
    },

    async getWorkflow(workflowFile) {
      return getJSON(
        `/repos/${repositoryPath}/actions/workflows/${encodeURIComponent(workflowFile)}`,
      )
    },

    async listWorkflowRuns({
      workflowId,
      defaultBranch,
      event,
      targetSha,
    }) {
      const canonicalWorkflowID = requirePositiveSafeInteger(
        workflowId,
        'workflow ID',
      )
      return readCollection({
        getJSON,
        kind: 'workflow-runs',
        field: 'workflow_runs',
        pathForPage(page) {
          const parameters = new URLSearchParams([
            ['branch', defaultBranch],
            ['event', event],
            ['status', 'success'],
            ['head_sha', targetSha],
            ['per_page', String(PAGE_SIZE)],
            ['page', String(page)],
          ])
          return `/repos/${repositoryPath}/actions/workflows/${canonicalWorkflowID}/runs?${parameters}`
        },
        requireNewestFirst: true,
      })
    },

    async getRunAttempt({ runId, runAttempt }) {
      const canonicalRunID = requirePositiveSafeInteger(runId, 'run ID')
      const canonicalAttempt = requirePositiveSafeInteger(
        runAttempt,
        'run attempt',
      )
      return getJSON(
        `/repos/${repositoryPath}/actions/runs/${canonicalRunID}/attempts/${canonicalAttempt}`,
      )
    },

    async listAttemptJobs({ runId, runAttempt }) {
      const canonicalRunID = requirePositiveSafeInteger(runId, 'run ID')
      const canonicalAttempt = requirePositiveSafeInteger(
        runAttempt,
        'run attempt',
      )
      return readCollection({
        getJSON,
        kind: 'jobs',
        field: 'jobs',
        pathForPage(page) {
          const parameters = new URLSearchParams([
            ['per_page', String(PAGE_SIZE)],
            ['page', String(page)],
          ])
          return `/repos/${repositoryPath}/actions/runs/${canonicalRunID}/attempts/${canonicalAttempt}/jobs?${parameters}`
        },
      })
    },

    async listRunArtifacts({ runId, artifactName }) {
      const canonicalRunID = requirePositiveSafeInteger(runId, 'run ID')
      if (typeof artifactName !== 'string' || artifactName.length === 0) {
        throw new GitHubActionsAPIError(
          'invalid-artifact-name',
          'artifact discovery name is missing',
        )
      }
      return readCollection({
        getJSON,
        kind: 'artifacts',
        field: 'artifacts',
        pathForPage(page) {
          const parameters = new URLSearchParams([
            ['name', artifactName],
            ['per_page', String(PAGE_SIZE)],
            ['page', String(page)],
          ])
          return `/repos/${repositoryPath}/actions/runs/${canonicalRunID}/artifacts?${parameters}`
        },
      })
    },

    async getRun(runId) {
      const canonicalRunID = requirePositiveSafeInteger(runId, 'run ID')
      return getJSON(`/repos/${repositoryPath}/actions/runs/${canonicalRunID}`)
    },
  })
}

async function readCollection({
  getJSON,
  kind,
  field,
  pathForPage,
  requireNewestFirst = false,
}) {
  let declaredCount
  let previousOrder
  const values = []
  const seenIDs = new Set()

  for (let page = 1; page <= MAXIMUM_LIST_PAGES; page += 1) {
    const response = await getJSON(pathForPage(page))
    requireRecord(response, kind)
    if (
      !Number.isSafeInteger(response.total_count) ||
      response.total_count < 0 ||
      response.total_count > MAXIMUM_COLLECTION_ITEMS ||
      !Array.isArray(response[field])
    ) {
      throw paginationError(
        kind,
        `${kind} response has an invalid total_count or ${field} collection`,
      )
    }
    if (declaredCount === undefined) declaredCount = response.total_count
    else if (response.total_count !== declaredCount) {
      throw paginationError(kind, `${kind} total_count changed during pagination`)
    }

    const pageValues = response[field]
    if (pageValues.length > PAGE_SIZE) {
      throw paginationError(kind, `${kind} page exceeds its requested size`)
    }
    if (pageValues.length === 0 && values.length !== declaredCount) {
      throw paginationError(kind, `${kind} pagination ended before its declared count`)
    }

    for (const [index, value] of pageValues.entries()) {
      requireRecord(value, `${kind} item ${values.length + index}`)
      if (!Number.isSafeInteger(value.id) || value.id < 1) {
        throw paginationError(kind, `${kind} item has an invalid ID`)
      }
      if (seenIDs.has(value.id)) {
        throw new GitHubActionsAPIError(
          kind === 'workflow-runs' ? 'duplicate-run' : 'duplicate-collection-item',
          `${kind} collection contains duplicate ID ${value.id}`,
        )
      }
      seenIDs.add(value.id)

      if (requireNewestFirst) {
        const currentOrder = {
          timestamp: requireRunTimestamp(value, kind),
          id: value.id,
        }
        if (previousOrder !== undefined && !precedes(previousOrder, currentOrder)) {
          throw paginationError(kind, `${kind} collection is not in newest-first order`)
        }
        previousOrder = currentOrder
      }
      values.push(value)
    }

    if (values.length > declaredCount) {
      throw paginationError(kind, `${kind} collection exceeds its declared count`)
    }
    if (values.length === declaredCount) return Object.freeze([...values])
  }

  throw paginationError(kind, `${kind} pagination exceeded its safety limit`)
}

function requireRunTimestamp(value, kind) {
  try {
    return requireCanonicalTimestamp(value.created_at, `${kind} created_at`)
  } catch (cause) {
    throw paginationError(kind, `${kind} item has an invalid created_at`, cause)
  }
}

function precedes(previous, current) {
  return previous.timestamp > current.timestamp ||
    previous.timestamp === current.timestamp && previous.id > current.id
}

function paginationError(kind, message, cause) {
  return new GitHubActionsAPIError('invalid-pagination', message, { cause, path: kind })
}

async function requestJSON({
  fetchImpl,
  token,
  path,
  requestTimeoutMilliseconds,
}) {
  const controller = new AbortController()
  let timeoutHandle
  let timedOut = false
  const timeout = new Promise((resolve, reject) => {
    timeoutHandle = setTimeout(() => {
      timedOut = true
      controller.abort()
      reject(new GitHubActionsAPIError(
        'github-request-timeout',
        `GitHub API request timed out for ${path}`,
        { path },
      ))
    }, requestTimeoutMilliseconds)
  })

  const operation = (async () => {
    let response
    try {
      response = await fetchImpl(`${GITHUB_API_ORIGIN}${path}`, {
        method: 'GET',
        redirect: 'error',
        headers: {
          Accept: 'application/vnd.github+json',
          Authorization: `Bearer ${token}`,
          'X-GitHub-Api-Version': GITHUB_ACTIONS_API_VERSION,
          'User-Agent': USER_AGENT,
        },
        signal: controller.signal,
      })
    } catch (cause) {
      if (timedOut) {
        throw new GitHubActionsAPIError(
          'github-request-timeout',
          `GitHub API request timed out for ${path}`,
          { path, cause },
        )
      }
      throw new GitHubActionsAPIError(
        'github-request-failed',
        `GitHub API request failed for ${path}`,
        { path, cause },
      )
    }
    if (
      response === null ||
      typeof response !== 'object' ||
      response.ok !== true ||
      !Number.isInteger(response.status) ||
      response.status < 200 ||
      response.status >= 300
    ) {
      const status = Number.isInteger(response?.status) ? response.status : 'unavailable'
      throw new GitHubActionsAPIError(
        'github-request-failed',
        `GitHub API request failed for ${path} with status ${status}`,
        { path, status },
      )
    }
    if (response.redirected === true || typeof response.json !== 'function') {
      throw new GitHubActionsAPIError(
        'invalid-github-response',
        `GitHub API response is invalid for ${path}`,
        { path },
      )
    }
    try {
      return await response.json()
    } catch (cause) {
      throw new GitHubActionsAPIError(
        'invalid-github-json',
        `GitHub API returned malformed JSON for ${path}`,
        { path, cause },
      )
    }
  })()

  try {
    return await Promise.race([operation, timeout])
  } finally {
    clearTimeout(timeoutHandle)
  }
}

function requireRecord(value, label) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new GitHubActionsAPIError(
      'invalid-pagination',
      `GitHub API ${label} must be an object`,
    )
  }
  return value
}
