# Recovering storage and image inspection failures

PolyGate runs containers as UID/GID `65532` with a read-only root filesystem.
The program files must be readable by that user. Use `/tmp` for disposable
files and a volume mounted at `/data` (or another allowed path) for persistent
data. Configure the application itself to write to the chosen path. Mounting a
volume over `/app` can hide the code packaged in the image.

## Correct a saved app's storage

In Overview, choose **Edit storage** for a Draft, Stopped, or Failed app. A failed
deployment must finish cleanup and release its resource reservations first.
Select a Ready volume owned by the same account; a volume can be bound to only
one app, and persistent apps must have one replica. Save, then deploy the
approved image again. This preserves the app ID, revision history, and all
existing volume data. Removing or changing a binding does not copy or erase data.

`PATCH /api/v1/apps/{id}/storage` accepts a complete storage setting:

```json
{"persistent":true,"volumeId":"<owned-ready-volume-uuid>","mountPath":"/data"}
```

To remove the binding, send `{"persistent":false}`. The response is the updated
App. The endpoint does not edit CPU/memory, secrets, volume capacity, or live
workloads. It rejects active work or unverified cleanup with
`APP_CLEANUP_PENDING`, and records changes as `UPDATE_APP_STORAGE` operations.

## Retry a failed image inspection

In My images, open **View failure**, address the reported cause, and choose
**Retry scan**. A Rejected image needs to be fixed and submitted as a new image;
retrying does not waive scanner policy or grant approval. Submitting the same
digest again returns its existing state and does not start another inspection.

`POST /api/v1/images/{id}/retry-scan` takes no body and requires an
`Idempotency-Key` of 8–128 non-space characters. It returns the ImageArtifact:
202 for a new retry, 200 when replaying the same successful request key.
Only an owned, active artifact with a completed Failed scan task can be retried.
The existing task starts a new bounded attempt cycle; prior failure operations
remain in Activity. Approval still comes only from the scanner.

## Release order

Deploy the API containing both endpoints before promoting the Web image. No
database migration is needed. Web remains on the normal GitOps promotion path;
do not change its image directly behind Flux. Verify the API's storage and
retry routes, then promote the Web commit and check the running image digest.
The CI PostgreSQL job uses a disposable database to exercise ownership,
reservations, cleanup, and concurrent retries in addition to ordinary unit tests.
