# PolyGate development handoff

Last checked: 2026-11-09. This is the entry point for contributors and coding agents. The source tree and the running cluster must be inspected again before making a release; historical planning documents describe earlier states.

## Product and current scope

PolyGate lets a student submit a public image reference, upload a Docker `save` tar, or upload a source ZIP; inspect build and Trivy results; deploy an approved image; and manage an app from the English console. The pilot uses local Keycloak accounts, not university SSO. Students may view logs and events, set account secrets, attach a volume, stop, restart, update, roll back, and delete an app. Administrators have account and quota controls.

The current release is a **trusted, invitation-only laboratory pilot**. Source Dockerfiles are built on a shared build host. Do not treat that path as isolation for malicious submissions. Local development processes are for testing only and are never part of production. Public domain, trusted certificate, external alert receiver, independent recovery-key copies, storage redundancy, and the separate untrusted-build design remain release work. Do not infer production readiness from a passing local test.

## Repository map

| Path | Responsibility |
| --- | --- |
| `web/src/App.tsx`, `web/src/ImagePage.tsx`, `web/src/HelpPage.tsx` | Student and administrator UI, forms, status and help. |
| `web/src/api.ts` | Typed browser API client, including chunked ZIP and tar uploads. |
| `cmd/api`, `internal/server` | HTTP routes, account ownership, request validation and orchestration. Routes are registered in `internal/server/server.go`. |
| `internal/auth` | OIDC login, server-side sessions, logout and CSRF. |
| `internal/postgres`, `db/migrations` | PostgreSQL state, transactions, leases, admission and audit. |
| `internal/source`, `internal/sourcebuild`, `cmd/sourcebuildworker` | ZIP validation/staging, durable build queue and source worker. |
| `internal/registry`, `internal/harbor`, `internal/imagescan`, `cmd/imagescanworker` | Registry resolution, per-account Harbor storage, Trivy scan and evidence. |
| `internal/deploy`, `internal/kubeobjects`, `cmd/worker` | Deployment queue, Kubernetes objects, reconciliation and cleanup. |
| `internal/appgateway`, `cmd/appgateway` | HTTPS app Host routing by immutable app ID. |
| `examples/polygate-mock-projects` | Three sample ZIP projects. |
| `deploy/`, `infra/dev/` | Cluster manifests and site-specific operational automation in the laboratory workspace. These are intentionally absent from the public export except for application image recipes and the login theme. |

The Go backend uses `net/http` routing. The frontend uses React, TypeScript, Vite, Ant Design and TanStack Query. PostgreSQL and Keycloak run on servers. Kubernetes runs the API, Web, workers, Harbor and student apps; the separate build host runs trusted builds. Do not rely on older design documents that describe a different gateway or build isolation as already deployed.

## Local development

Use Go **1.27** and Node.js **22+**. From the repository root:

```bash
go test ./...
go vet ./...
cd web
npm ci
npm run build
npm run dev
```

`npm run dev` starts a loopback-only in-memory API and the Vite console at `http://127.0.0.1:5173/`. It exercises the UI and request shapes, but does not provide real Keycloak, PostgreSQL, Harbor, builds or deployments. The Vite server proxies `/api` and `/auth` to the local API on port 8080. Stop it with Ctrl+C. A separate authenticated local setup needs local PostgreSQL and Keycloak; laboratory maintainers can use the setup scripts in `infra/dev/` and local credentials outside the repository.

Database integration tests are enabled only when their `POLYGATE_TEST_DATABASE_URL` or dedicated test URL is supplied; otherwise `go test ./...` may skip them. For a database change, run the affected integration tests against a disposable database in addition to the normal test suite. Do not point test URLs at the production database.

## How a submission reaches an app

1. **Source ZIP:** `web/src/api.ts` submits 8 MiB chunks through `internal/server/source_builds.go`. `internal/source` validates ZIP paths and limits (100 MiB compressed, 1 GiB expanded). PostgreSQL holds a build task; `cmd/sourcebuildworker` asks the trusted build agent to build and pushes the result to Harbor. Build log and status remain visible in Source ZIP.
2. **Prebuilt image:** a public reference goes through the registry resolver; a local Docker `save` `.tar` goes through the 8 MiB upload endpoints in `internal/server/image_uploads.go` and `internal/harbor/import_tar.go`. A tar may be at most 1 GiB and must contain one `linux/amd64` image. The platform keeps an account-scoped Harbor reference.
3. **Scan:** `cmd/imagescanworker` runs Harbor Trivy and records the digest, scan ID, report hash, severity counts and decision. Policy `severity-v2` rejects Critical, Unknown and unclassified findings; High findings remain visible but are allowed for the trusted pilot. Students can view and download completed reports from **My images**. New reports are archived in PostgreSQL with a SHA-256 check; older reports are read from Harbor and archived on first access. `internal/server/image_reports.go` checks ownership. The private release gate `verify-image-scan-archive.py` checks archive completeness, hashes and approved policy versions.

The frontend architecture and developer workflow are documented in [web/FRONTEND.md](web/FRONTEND.md).
4. **Deploy:** `internal/postgres` checks the image is Approved and reserves account/global resources in a transaction. `cmd/worker` reconciles Kubernetes Deployment and Service objects using the pinned image digest. The UI follows the task through to Running or Failed. Revisions, stop and cleanup use the same account-scoped task machinery.

For API changes, keep validation and owner checks at the server boundary, preserve idempotency on writes, and make database and worker state changes recoverable after retries. New Kubernetes objects must carry account and app ID labels so the gateway and cleanup logic can verify ownership.

## App name, URL and port

The student chooses an **App name** for display. It is unique within that account, 1–40 lowercase letters/digits/hyphens, starts with a letter, and cannot contain `--`. A new app also gets an immutable random UUID. The app URL is `https://<app-id>.<configured-app-domain>/`; it is based on that UUID, not the display name or username. Kubernetes resource names use `app-` plus the UUID without hyphens. A student's two apps therefore have two URLs.

**App port is the HTTP listening port inside the container.** It must match the program's real port and the process must bind `0.0.0.0`; a browser port, host port, or Dockerfile `EXPOSE` guess will not work. Sample ZIPs use Python `8000`, Node.js `3000`, and Dockerfile `8080`. The optional HTTP health path must return 2xx/3xx while healthy; otherwise leave it empty for a TCP check. A failed app can use **Edit port**, then **Deploy image** with the same approved image.

One container may request at most 4096 MiB; account quotas and the global 100 GiB / 60 Pod budget also apply. A draft or stopped app still counts against configured account quota. See `internal/domain/resources.go` and the account/cluster admission code before changing resource behavior.

## Guide and frontend updates

The source of the illustrated student guides is `docs/PolyGate_Student_Walkthrough_EN.md` and `docs/PolyGate_学生全流程图文指南_ZH.md` in the laboratory workspace. The online pages are generated as `web/public/student-guide-en.html` and `web/public/student-guide-zh.html`. After changing the Markdown, run `python infra/dev/package-student-guides.py` from the laboratory workspace, build the Web app, and verify both pages and screenshots in a browser. Also update `web/src/HelpPage.tsx` and the relevant form text when instructions change. Do not edit only the generated HTML.

## Verification and release

1. Run the Go unit suite and vet, plus relevant database integration tests. Run `npm run build` in `web/`. For a user-facing flow, use a real browser against a test account and check both success and failure paths; `web/scripts/` contains pilot smoke tests.
2. Review migrations and deployment ordering. Take and verify a database backup before a production schema change. Apply migrations before deploying code that requires the new schema. The laboratory-only scripts include `infra/dev/migrate-platform-db.py`, `build-deploy-api.py`, `build-deploy-web.py`, and worker deploy scripts.
3. Deploy with rolling readiness checks, then run `python infra/dev/verify-v01-release.py` from the laboratory workspace. It checks the latest natural health Job, an independent entry probe, node capacity, pilot accounts, queue readiness and HTTPS/OIDC. This is a quick gate; it does not replace a browser end-to-end test of a changed workflow.
4. Check the API/worker logs and leave no temporary apps, test identities, reservations or jobs. Keep a dated record of what changed, exact tests, runtime outcome and rollback route in the private operational docs. Export reviewed code and contributor docs to the public repository only after checking for credentials and site-specific operational details.

Never commit account passwords, kubeconfig, private keys, Harbor credentials, backup files, student source archives, or production logs. Credentials belong in the laboratory's private delivery channel. The public repository is for code collaboration; cluster access and deployment authority are handled separately.

## Next work and known boundaries

- Obtain and switch to a laboratory-controlled external domain and trusted TLS certificate; recheck OIDC issuer, callback, logout, app hosts and browser access.
- Provide an external alert receiver and verify failure delivery and recovery notifications. Current probes write to cluster or backup-host logs but cannot notify an unattended operator.
- Make a second independently recoverable copy of backup decryption keys and exercise restore from it. Add storage redundancy when a second suitable disk is available.
- Complete controlled host maintenance and the isolated build path before accepting malicious or unknown Dockerfiles. The trusted pilot does not cover this threat model.
- Finish the formal acceptance matrix and scale-up decision for approximately 50 students. Existing 50-account and 4 GiB stress tests cover selected scenarios, not every production failure mode.
- Completed scan reports are archived with their SHA-256 digest in PostgreSQL. The private release gate `verify-image-scan-archive.py` checks active report completeness, stored hashes and the policy version of approved images. A 2026-11-09 logical backup was restored into disposable databases; a specified image report and its SHA-256 matched. Production database failover has not been exercised.

For the latest site-specific state, use the laboratory workspace `README.md`, `docs/PolyGate_下一阶段计划_集群试点到参赛开放.md`, recent dated acceptance records, and the live release gate. Treat dated records as evidence for their stated time, then inspect the current state again.
