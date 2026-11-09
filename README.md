# PolyGate

PolyGate is a container platform for student agent projects. Students can submit a public container image or a source ZIP, review build and scan results, and deploy approved images with resource limits. The console is written in React and TypeScript; the API and workers are written in Go.

## Local development

Requirements: Go 1.27, Node.js 22 or later, and npm.

```bash
go test ./...
cd web
npm ci
npm run build
npm run dev
```

`npm run dev` starts a loopback development API and Vite frontend. It does not connect to the laboratory cluster. Visit `http://127.0.0.1:5173/`.

The application packages are in `cmd/` and `internal/`; PostgreSQL migrations are in `db/migrations/`; the student console is in `web/`. `examples/polygate-mock-projects/` contains small test projects. The `deploy/` directory here contains application image recipes and the Keycloak login theme. Site-specific Kubernetes manifests, host access details, deployment automation, and credentials are maintained separately.

For apps served at `/<username>/<app-name>/`, read the [base path compatibility guide](docs/PolyGate_学生作品路径适配.md). It includes a FastAPI example, a patch for the workshop demo, and a browser screenshot.

## Contributing

Start with [DEVELOPMENT.md](DEVELOPMENT.md) or [中文开发交接文档](DEVELOPMENT.zh-CN.md), then see [CONTRIBUTING.md](CONTRIBUTING.md). Never commit passwords, tokens, private keys, kubeconfigs, account exports, or production snapshots. No public license has been selected yet; please ask the maintainers before reusing this code outside the project.
