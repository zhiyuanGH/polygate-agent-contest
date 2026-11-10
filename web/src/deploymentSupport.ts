import type { App, AppStorage, ImageArtifact, Volume } from './api'

type ScanState = Pick<ImageArtifact, 'id' | 'scanStatus' | 'scanTaskState'>

export const scanObservationLimitMs = 30 * 60 * 1000

export function observePendingScans(items: ScanState[], previous: Record<string, number>, now: number): Record<string, number> {
  return Object.fromEntries(items.filter(item => item.scanStatus === 'PendingScan')
    .map(item => [item.id, previous[item.id] ?? now]))
}

export function shouldPollScans(items: ScanState[], observedAt: Record<string, number>, now: number): boolean {
  return items.some(item => item.scanStatus === 'PendingScan' &&
    (observedAt[item.id] === undefined || now - observedAt[item.id] < scanObservationLimitMs))
}

export function inspectionFeedback(item: ScanState): { type: 'success' | 'info' | 'warning'; message: string } {
  switch (item.scanStatus) {
    case 'Approved': return { type: 'success', message: 'This image is approved and ready to deploy.' }
    case 'Rejected': return { type: 'warning', message: 'This image did not pass the security policy. Open View report and fix the image before submitting it again.' }
    case 'Failed': return { type: 'warning', message: 'This image has a failed inspection. Open View failure, then use Retry scan after addressing the cause.' }
    case 'PendingScan': return { type: 'info', message: item.scanTaskState === 'Leased'
      ? 'Security inspection is running. Follow its status below.'
      : 'This image is awaiting security inspection. Follow its status below.' }
  }
}

export function availableStorageVolumes(volumes: Volume[], apps: App[], editingAppId?: string): Volume[] {
  const usedByOtherApps = new Set(apps.filter(app => app.id !== editingAppId).map(app => app.spec.volumeId).filter(Boolean))
  return volumes.filter(volume => volume.state === 'Ready' && !usedByOtherApps.has(volume.id))
}

export function storagePayload(values: AppStorage): AppStorage {
  return values.persistent
    ? { persistent: true, volumeId: values.volumeId, mountPath: values.mountPath }
    : { persistent: false }
}
