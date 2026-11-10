import assert from 'node:assert/strict'
import test from 'node:test'
import { availableStorageVolumes, inspectionFeedback, observePendingScans, scanObservationLimitMs, shouldPollScans, storagePayload } from '../src/deploymentSupport.ts'

test('failed and rejected replays never claim that a new scan was queued', () => {
  for (const scanStatus of ['Failed', 'Rejected']) {
    const feedback = inspectionFeedback({ id: 'image', scanStatus })
    assert.equal(feedback.type, 'warning')
    assert.doesNotMatch(feedback.message, /queued|awaiting|running/i)
  }
  assert.match(inspectionFeedback({ id: 'image', scanStatus: 'Failed' }).message, /Retry scan/)
  assert.match(inspectionFeedback({ id: 'image', scanStatus: 'Approved' }).message, /approved/i)
})

test('scan feedback distinguishes queued work from an already running inspection', () => {
  assert.match(inspectionFeedback({ id: 'image', scanStatus: 'PendingScan', scanTaskState: 'Queued' }).message, /awaiting/i)
  assert.match(inspectionFeedback({ id: 'image', scanStatus: 'PendingScan', scanTaskState: 'Leased' }).message, /running/i)
})

test('an old artifact retry receives a fresh polling window without relying on createdAt', () => {
  const start = 10_000_000
  const pending = [{ id: 'old-image', scanStatus: 'PendingScan', createdAt: '2020-01-01T00:00:00Z' }]
  const observed = observePendingScans(pending, {}, start)
  assert.equal(shouldPollScans(pending, observed, start + scanObservationLimitMs - 1), true)
  assert.equal(shouldPollScans(pending, observed, start + scanObservationLimitMs), false)
  const finished = observePendingScans([{ id: 'old-image', scanStatus: 'Failed' }], observed, start + scanObservationLimitMs)
  assert.deepEqual(finished, {})
  const retryTime = start + scanObservationLimitMs + 5_000
  const retry = observePendingScans(pending, finished, retryTime)
  assert.equal(shouldPollScans(pending, retry, retryTime), true)
  assert.equal(retry['old-image'], retryTime)
})

test('continued polling does not extend a pending scan indefinitely', () => {
  const pending = [{ id: 'image', scanStatus: 'PendingScan' }]
  const first = observePendingScans(pending, {}, 100)
  const later = observePendingScans(pending, first, 10_000)
  assert.equal(later.image, 100)
  assert.equal(shouldPollScans([{ id: 'image', scanStatus: 'Approved' }], later, 11_000), false)
})

test('editing storage retains the app own volume but excludes other bindings and unready volumes', () => {
  const volumes = [
    { id: 'own', state: 'Ready' }, { id: 'other', state: 'Ready' },
    { id: 'free', state: 'Ready' }, { id: 'waiting', state: 'Provisioning' },
  ]
  const apps = [{ id: 'editing', spec: { volumeId: 'own' } }, { id: 'another', spec: { volumeId: 'other' } }]
  assert.deepEqual(availableStorageVolumes(volumes, apps, 'editing').map(volume => volume.id), ['own', 'free'])
  assert.deepEqual(availableStorageVolumes(volumes, apps).map(volume => volume.id), ['free'])
})

test('turning persistence off removes stale hidden form bindings from the request', () => {
  assert.deepEqual(storagePayload({ persistent: false, volumeId: 'previous-volume', mountPath: '/data' }), { persistent: false })
  assert.deepEqual(storagePayload({ persistent: true, volumeId: 'selected-volume', mountPath: '/data' }),
    { persistent: true, volumeId: 'selected-volume', mountPath: '/data' })
})
