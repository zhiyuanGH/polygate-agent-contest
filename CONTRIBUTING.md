# Contributing to PolyGate

1. Open an issue describing the change, or discuss it with a maintainer.
2. Create a branch from `main` and keep each pull request focused.
3. Run `go test ./...` and `cd web && npm ci && npm run build` before opening a pull request.
4. Explain the user-facing behavior, tests, and any database migration in the pull request.

Do not commit secrets, account information, production cluster addresses, kubeconfigs, or generated build artifacts. Local development runs on loopback; deployment to the lab cluster is handled by maintainers.
