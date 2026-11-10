export type Resources = {
  cpuRequestMilli: number
  cpuLimitMilli: number
  memoryMiB: number
  replicas: number
}

export type AppSpec = {
  name: string
  image: string
  port: number
  healthPath?: string
  resources: Resources
  persistent: boolean
  volumeId?: string
  mountPath?: string
  secretKeys?: string[]
}

export type AppStorage = { persistent: boolean; volumeId?: string; mountPath?: string }

export type VolumeSpec = { name: string; sizeGiB: number }
export type Volume = {
  id: string
  spec: VolumeSpec
  state: 'Reserved' | 'Provisioning' | 'Ready' | 'Expanding' | 'Failed' | 'Deleting'
  createdAt: string
}

export type App = {
  id: string
  spec: AppSpec
  accessMode: 'Public' | 'Owner and administrators'
  status: 'Draft' | 'Queued' | 'Building' | 'Deploying' | 'Running' | 'Stopping' | 'Stopped' | 'Failed' | 'Deleting'
  createdAt: string
}

export type DeploymentStatus = {
  appId: string
  status: App['status']
  taskId?: string
  taskState?: string
  attempt?: number
  errorCode?: string
}

export type Operation = {
  id: string
  appId?: string
  kind: string
  state: string
  detail?: { errorCode?: string; taskId?: string }
  createdAt: string
}

export type AppLog = { pod: string; data: string; previous: boolean }
export type AppPod = { name: string; phase: string; ready: boolean; restarts: number; reason?: string; message?: string; waitingReason?: string; lastTerminationReason?: string; lastExitCode?: number }
export type AppLogHistory = { items: { timestamp: string; pod: string; container: string; line: string }[]; rangeHours: number; nextBefore?: string }
export type AppEvent = { type: string; reason: string; message: string; object: string; count: number; at: string }
export type PodMetric = { pod: string; cpuMilli: number; memoryBytes: number; timestamp: string; window: string }
export type AccountActualUsage = { cpuMilli: number; memoryBytes: number; sampledPods: number; expectedPods: number; complete: boolean; checkedAt: string }

export type DeploymentTask = {
  id: string
  operationId: string
  appId: string
  desiredState: 'Running' | 'Stopped'
  state: string
}

export type Revision = {
  id: string
  number: number
  imageDigest: string
  sourceKind: string
  imageArtifactId?: string
  successful: boolean
  current: boolean
  createdAt: string
}

export type Usage = {
  cpuRequestMilli: number
  cpuLimitMilli: number
  memoryMiB: number
  pods: number
  apps: number
}

export type ClusterNode = {
  name: string
  cpuUsedPercent: number
  memoryUsedBytes: number
  memoryTotalBytes: number
  rootAvailableBytes: number
  rootTotalBytes: number
}

export type ClusterResources = { nodes: ClusterNode[]; sampledAt: string }

export type Me = {
  id: string
  username?: string
  displayName: string
  role: 'student' | 'admin'
  mode: 'loopback-development' | 'authenticated'
  quota: Usage
  configured: Usage
  message: string
  trustedDeploymentAvailable: boolean
  imageDeploymentAvailable: boolean
  sourceBuildAvailable: boolean
  volumeAvailable: boolean
	appLogsAvailable: boolean
	appLogHistoryAvailable: boolean
	adminQuotaAvailable: boolean
	adminProvisionAvailable: boolean
	secretAvailable: boolean
	appAccessBaseURL?: string
	appAccessRouting?: 'host' | 'path'
	privateAppDomain?: string
}

export type AdminApp = {
  id: string
  ownerId: string
  username: string
  name: string
  status: App['status']
  accessMode: App['accessMode']
  createdAt: string
}

export type AdminAppAccessAudit = {
  id: string
  actorSubject: string
  username: string
  appId: string
  appName: string
  previousMode: App['accessMode']
  newMode: App['accessMode']
  reason: string
  createdAt: string
}

export type AdminAccount = {
  id: string
  username: string
  displayName: string
  role: 'student' | 'admin'
  enabled: boolean
  quota: Usage
  imageDeployEnabled: boolean
  sourceBuildEnabled: boolean
  volumeEnabled: boolean
  appCount: number
  createdAt: string
}

export type AdminAccountAudit = {
  id: string
  actorAccountId: string
  actorUsername: string
  targetAccountId: string
  action: 'ENABLE_ACCOUNT' | 'DISABLE_ACCOUNT'
  reason: string
  createdAt: string
}

export type QuotaChange = {
  id: string
  actorAccountId: string
  targetAccountId: string
  quota: Usage
  state: 'Queued' | 'Applied'
  reason: string
  lastError?: string
  createdAt: string
  appliedAt?: string
}

export type AdminQuotaPreview = {
  targetAccountId: string
  currentQuota: Usage
  proposedQuota: Usage
  configured: Usage
  overQuota: (keyof Usage)[]
  apps: { id: string; name: string; status: App['status']; resources: Resources }[]
  checkedAt: string
}

export type SourceBuild = {
  id: string
  artifactId: string
  sourceSha256: string
  sizeBytes: number
  state: 'Preparing' | 'Queued' | 'Leased' | 'Succeeded' | 'Failed'
  attempt: number
  errorCode?: string
  resultImageArtifactId?: string
  resultImageDeletedAt?: string
  createdAt: string
}

export type SourceBuildLog = { data: string; nextOffset: number; state: SourceBuild['state']; retained: boolean }
export type BuildTemplate = 'dockerfile' | 'python' | 'node'

export type ImageArtifact = {
  id: string
  submittedReference: string
  pinnedReference: string
  platformDigest: string
  registryHost: string
  configuredUser: string
  scanStatus: 'PendingScan' | 'Approved' | 'Rejected' | 'Failed'
  scanTaskState?: string
  scanErrorCode?: string
  scanSummary?: { total: number; fixable: number; severities: Record<string, number>; policyVersion: string }
  createdAt: string
}

export type ImageScanFailure = { artifactId: string; status: 'Failed'; errorCode: string; attempts: number; checkedAt: string; summary: string }

const errorHelp: Record<string, string> = {
  APP_NAME_TAKEN: 'Choose another app name, or update the existing app from Overview.',
  APP_NOT_DEPLOYABLE: 'Check the app status on Overview, then retry from a draft or stopped app.',
  BUILD_NOT_ENABLED: 'Ask a platform administrator to enable source builds for your account.',
  BUILD_QUEUE_FULL: 'Wait for a running build to finish, then submit again.',
  IMAGE_ARTIFACT_LIMIT: 'Remove an unused image from My images before submitting another.',
  IMAGE_NOT_APPROVED: 'Open My images and choose an image marked Approved.',
  IMAGE_SCAN_IN_PROGRESS: 'Wait for the security check to finish, then refresh My images.',
  IMAGE_STILL_IN_USE: 'Stop and delete the app using this image before removing it.',
  INVALID_PORT: 'Enter the HTTP port your program listens on inside the container.',
  INVALID_SOURCE: 'Check the ZIP layout and selected build method, then submit the ZIP again.',
  QUOTA_EXCEEDED: 'Reduce CPU, memory or replicas, or remove an unused app.',
  GLOBAL_QUOTA_EXCEEDED: 'The cluster is at capacity. Try again later or contact the lab administrator.',
  SOURCE_STORAGE_FULL: 'Remove unused source build files or ask the lab administrator for space.',
  TASK_BUSY: 'Another change is still running. Wait for it to finish, then retry.',
  VOLUME_ALREADY_BOUND: 'Choose another Ready volume; one volume can be attached to one app at a time.',
  VOLUME_NOT_READY: 'Wait until the volume shows Ready on the Volumes page.',
  VOLUME_QUOTA_EXCEEDED: 'Delete an unused volume or request a smaller size. Deleting volumes count until removal finishes.',
}

export function errorGuidance(code: string): string | undefined { return errorHelp[code] }

export class ApiError extends Error {
  constructor(public code: string, public field: string | undefined, message: string) {
    super(errorHelp[code] ? `${message} ${errorHelp[code]}` : message)
  }
}

async function request<T>(path: string, options?: RequestInit): Promise<T> {
  const unavailableMessage = import.meta.env.DEV
    ? 'The local API is offline. Start the Go API, or restart the console with npm run dev from the web directory.'
    : 'The service is temporarily unavailable. Please try again.'
  let response: Response
  try {
    const headers = new Headers(options?.headers)
    if (options?.method && !['GET', 'HEAD'].includes(options.method.toUpperCase())) {
      headers.set('X-CSRF-Token', await csrfToken())
    }
    response = await fetch(`/api/v1${path}`, { credentials: 'same-origin', ...options, headers })
  } catch {
    throw new ApiError('API_UNAVAILABLE', undefined, unavailableMessage)
  }
  let body: { error?: { code?: string; field?: string; message?: string } }
  try {
    body = await response.json()
  } catch {
    throw new ApiError(response.ok ? 'INVALID_API_RESPONSE' : 'API_UNAVAILABLE', undefined,
      response.ok ? 'The API returned an invalid response. Please try again.' : unavailableMessage)
  }
  if (!response.ok) {
    const error = body.error ?? {}
    throw new ApiError(error.code ?? 'REQUEST_FAILED', error.field, error.message ?? 'Request failed.')
  }
  return body as T
}

async function csrfToken(): Promise<string> {
  const response = await fetch('/auth/csrf', { credentials: 'same-origin' })
  if (!response.ok) throw new ApiError('AUTH_REQUIRED', undefined, 'Sign in to continue.')
  const body = await response.json() as { token?: string }
  if (!body.token) throw new ApiError('INVALID_API_RESPONSE', undefined, 'Could not verify this request.')
  return body.token
}

export const api = {
  me: () => request<Me>('/me'),
  accountActualUsage: () => request<AccountActualUsage>('/usage/actual'),
  adminAccounts: async () => (await request<{ items: AdminAccount[] }>('/admin/accounts')).items,
  adminApps: async () => (await request<{ items: AdminApp[] }>('/admin/apps')).items,
  adminAppAccessAudit: async () => (await request<{ items: AdminAppAccessAudit[] }>('/admin/app-access-audit')).items,
  setAdminAppAccess: ({ id, mode, reason }: { id: string; mode: App['accessMode']; reason: string }) =>
    request<AdminApp>(`/admin/apps/${encodeURIComponent(id)}/access`, {
      method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ mode, reason }),
    }),
  adminClusterResources: () => request<ClusterResources>('/admin/cluster-resources'),
  createAdminAccount: (input: { username: string; displayName: string; reason: string }) =>
    request<{ account: AdminAccount; temporaryPassword: string }>('/admin/accounts', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(input),
    }),
  resetAdminPassword: ({ id, reason }: { id: string; reason: string }) =>
    request<{ temporaryPassword: string }>(`/admin/accounts/${encodeURIComponent(id)}/reset-password`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ reason }),
    }),
  adminAccountAudit: async () => (await request<{ items: AdminAccountAudit[] }>('/admin/account-audit')).items,
  adminQuotaChanges: async () => (await request<{ items: QuotaChange[] }>('/admin/quota-changes')).items,
  adminQuotaPreview: ({ id, quota }: { id: string; quota: Usage }) =>
    request<AdminQuotaPreview>(`/admin/accounts/${encodeURIComponent(id)}/quota-preview`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ quota }),
    }),
  queueAdminQuotaChange: ({ id, quota, reason }: { id: string; quota: Usage; reason: string }) =>
    request<QuotaChange>(`/admin/accounts/${encodeURIComponent(id)}/quota`, {
      method: 'POST', headers: { 'Content-Type': 'application/json', 'Idempotency-Key': crypto.randomUUID() },
      body: JSON.stringify({ quota, reason }),
    }),
  setAdminAccountEnabled: ({ id, enabled, reason }: { id: string; enabled: boolean; reason: string }) =>
    request<AdminAccount>(`/admin/accounts/${encodeURIComponent(id)}/enabled`, {
      method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ enabled, reason }),
    }),
  images: async () => (await request<{ items: ImageArtifact[] }>('/images')).items,
  imageScanFailure: (id: string) => request<ImageScanFailure>(`/images/${encodeURIComponent(id)}/scan-failure`),
  inspectImage: (reference: string) => request<ImageArtifact>('/images/inspect', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ reference }),
  }),
  retryImageScan: (id: string) => request<ImageArtifact>(`/images/${encodeURIComponent(id)}/retry-scan`, {
    method: 'POST', headers: { 'Idempotency-Key': crypto.randomUUID() },
  }),
  imageScanReport: (id: string) => request<Record<string, unknown>>(`/images/${encodeURIComponent(id)}/scan-report`),
  uploadImageTar: async (file: File, onProgress?: (percent: number) => void) => {
    if (!file.name.toLowerCase().endsWith('.tar') || file.size < 1 || file.size > 1024 * 1024 * 1024) {
      throw new ApiError('INVALID_IMAGE_UPLOAD', undefined, 'Choose a docker save .tar file of at most 1 GiB.')
    }
    const upload = await request<{ id: string; chunkBytes: number }>('/images/uploads', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sizeBytes: file.size }),
    })
    if (upload.chunkBytes < 1 || upload.chunkBytes > 8 * 1024 * 1024) {
      throw new ApiError('INVALID_API_RESPONSE', undefined, 'The upload service returned an invalid chunk size.')
    }
    for (let offset = 0, index = 0; offset < file.size; offset += upload.chunkBytes, index++) {
      const part = file.slice(offset, Math.min(offset + upload.chunkBytes, file.size))
      await request<{ received: number }>(`/images/uploads/${encodeURIComponent(upload.id)}/chunks/${index}`, {
        method: 'PUT', body: part,
      })
      onProgress?.(Math.round(Math.min(offset + part.size, file.size) / file.size * 100))
    }
    return request<ImageArtifact>(`/images/uploads/${encodeURIComponent(upload.id)}/complete`, { method: 'POST' })
  },
  removeImage: (id: string) => request<{ state: string }>(`/images/${encodeURIComponent(id)}`, {
    method: 'DELETE',
  }),
  apps: async () => (await request<{ items: App[] }>('/apps')).items,
  secrets: async () => (await request<{ items: { name: string }[] }>('/secrets')).items,
  putSecret: ({ name, value }: { name: string; value: string }) => request<{ name: string }>(`/secrets/${encodeURIComponent(name)}`, {
    method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ value }),
  }),
  deleteSecret: (name: string) => request<{ deleted: boolean }>(`/secrets/${encodeURIComponent(name)}`, { method: 'DELETE' }),
  volumes: async () => (await request<{ items: Volume[] }>('/volumes')).items,
  createVolume: (spec: VolumeSpec) => request<Volume>('/volumes', {
    method: 'POST', headers: { 'Content-Type': 'application/json', 'Idempotency-Key': crypto.randomUUID() },
    body: JSON.stringify(spec),
  }),
  expandVolume: ({ id, sizeGiB }: { id: string; sizeGiB: number }) => request<Volume>(`/volumes/${encodeURIComponent(id)}`, {
    method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ sizeGiB }),
  }),
  deleteVolume: ({ id, confirmName }: { id: string; confirmName: string }) => request<Volume>(`/volumes/${encodeURIComponent(id)}`, {
    method: 'DELETE', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ confirmName }),
  }),
  createApp: (spec: AppSpec) => request<App>('/apps', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'Idempotency-Key': crypto.randomUUID() },
    body: JSON.stringify(spec),
  }),
  updateAppResources: ({ appId, resources }: { appId: string; resources: Resources }) =>
    request<App>(`/apps/${encodeURIComponent(appId)}/resources`, {
      method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ resources }),
    }),
  updateAppRuntime: ({ appId, port, healthPath }: { appId: string; port: number; healthPath: string }) =>
    request<App>(`/apps/${encodeURIComponent(appId)}/runtime`, {
      method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ port, healthPath }),
    }),
  updateAppStorage: ({ appId, storage }: { appId: string; storage: AppStorage }) =>
    request<App>(`/apps/${encodeURIComponent(appId)}/storage`, {
      method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(storage),
    }),
  updateRunningResources: ({ appId, resources, strategy }: { appId: string; resources: Resources; strategy: 'Rolling' | 'Recreate' }) =>
    request<DeploymentTask>(`/apps/${encodeURIComponent(appId)}/update-resources`, {
      method: 'POST', headers: { 'Content-Type': 'application/json', 'Idempotency-Key': crypto.randomUUID() },
      body: JSON.stringify({ resources, strategy }),
    }),
  restartApp: ({ appId, strategy }: { appId: string; strategy: 'Rolling' | 'Recreate' }) =>
    request<DeploymentTask>(`/apps/${encodeURIComponent(appId)}/restart`, {
      method: 'POST', headers: { 'Content-Type': 'application/json', 'Idempotency-Key': crypto.randomUUID() },
      body: JSON.stringify({ strategy }),
    }),
  appStatus: (id: string) => request<DeploymentStatus>(`/apps/${encodeURIComponent(id)}/status`),
  operations: async () => (await request<{ items: Operation[] }>('/operations')).items,
  operationHistory: (before = '') => request<{ items: Operation[]; nextCursor?: string }>(
    `/operations/history${before ? `?before=${encodeURIComponent(before)}` : ''}`),
  appRevisions: async (id: string) => (await request<{ items: Revision[] }>(`/apps/${encodeURIComponent(id)}/revisions`)).items,
  appPods: async (id: string) => (await request<{ items: AppPod[] }>(`/apps/${encodeURIComponent(id)}/pods`)).items,
  appLogs: (id: string, pod?: string, previous = false) => request<AppLog>(`/apps/${encodeURIComponent(id)}/logs${pod || previous ? `?${new URLSearchParams({ ...(pod ? { pod } : {}), ...(previous ? { previous: '1' } : {}) })}` : ''}`),
  appLogHistory: (id: string, hours: 24 | 168, before = '', pod = '') => request<AppLogHistory>(`/apps/${encodeURIComponent(id)}/logs/history?hours=${hours}${before ? `&before=${encodeURIComponent(before)}` : ''}${pod ? `&pod=${encodeURIComponent(pod)}` : ''}`),
  appEvents: async (id: string) => (await request<{ items: AppEvent[] }>(`/apps/${encodeURIComponent(id)}/events`)).items,
  appMetrics: async (id: string) => (await request<{ items: PodMetric[] }>(`/apps/${encodeURIComponent(id)}/metrics`)).items,
  deployApp: (id: string) => request<DeploymentTask>(`/apps/${encodeURIComponent(id)}/deploy`, {
    method: 'POST', headers: { 'Idempotency-Key': crypto.randomUUID() },
  }),
  deployImage: ({ appId, artifactId }: { appId: string; artifactId: string }) =>
    request<DeploymentTask>(`/apps/${encodeURIComponent(appId)}/deploy-image`, {
      method: 'POST', headers: { 'Content-Type': 'application/json', 'Idempotency-Key': crypto.randomUUID() },
      body: JSON.stringify({ artifactId }),
    }),
  updateImage: ({ appId, artifactId }: { appId: string; artifactId: string }) =>
    request<DeploymentTask>(`/apps/${encodeURIComponent(appId)}/update-image`, {
      method: 'POST', headers: { 'Content-Type': 'application/json', 'Idempotency-Key': crypto.randomUUID() },
      body: JSON.stringify({ artifactId }),
    }),
  rollbackRevision: ({ appId, revisionId }: { appId: string; revisionId: string }) =>
    request<DeploymentTask>(`/apps/${encodeURIComponent(appId)}/rollback`, {
      method: 'POST', headers: { 'Content-Type': 'application/json', 'Idempotency-Key': crypto.randomUUID() },
      body: JSON.stringify({ revisionId }),
    }),
  stopApp: (id: string) => request<DeploymentTask>(`/apps/${encodeURIComponent(id)}/stop`, {
    method: 'POST', headers: { 'Idempotency-Key': crypto.randomUUID() },
  }),
  validateSource: (file: File) => {
    const form = new FormData()
    form.append('source', file)
    return request<{ valid: boolean; sizeBytes: number; message: string }>('/source/validate', { method: 'POST', body: form })
  },
  sourceBuilds: async () => (await request<{ builds: SourceBuild[] }>('/source/builds')).builds,
  sourceBuildLog: (id: string, offset: number) => request<SourceBuildLog>(`/source/builds/${encodeURIComponent(id)}/log?offset=${offset}`),
  submitSourceBuild: async (file: File, template: BuildTemplate) => {
    if (file.size <= 8 * 1024 * 1024) {
      const form = new FormData()
      form.append('source', file)
      form.append('template', template)
      return request<SourceBuild>('/source/builds', { method: 'POST', body: form })
    }
    const upload = await request<{ id: string; chunkBytes: number }>('/source/uploads', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sizeBytes: file.size, template }),
    })
    if (upload.chunkBytes < 1 || upload.chunkBytes > 8 * 1024 * 1024) {
      throw new ApiError('INVALID_API_RESPONSE', undefined, 'The upload service returned an invalid chunk size.')
    }
    for (let offset = 0, index = 0; offset < file.size; offset += upload.chunkBytes, index++) {
      const part = file.slice(offset, Math.min(offset + upload.chunkBytes, file.size))
      await request<{ received: number }>(`/source/uploads/${encodeURIComponent(upload.id)}/chunks/${index}`, {
        method: 'PUT', body: part,
      })
    }
    return request<SourceBuild>(`/source/uploads/${encodeURIComponent(upload.id)}/complete`, { method: 'POST' })
  },
  deleteApp: (id: string) => request<{ deleted: boolean }>(`/apps/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  logout: async () => {
    const response = await fetch('/auth/logout', { method: 'POST', credentials: 'same-origin', headers: { 'X-CSRF-Token': await csrfToken() } })
    if (!response.ok) throw new ApiError('SIGN_OUT_FAILED', undefined, 'Could not sign out. Please try again.')
  },
}
