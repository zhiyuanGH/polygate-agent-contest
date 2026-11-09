# PolyGate sample source ZIPs

These three small HTTP services are for testing the **Source ZIP** workflow. Each `dist/*.zip` file has its required files at the archive root. They use no cloud credentials or external API.

Invited students can download the same three ZIPs directly from the PolyGate **Help** page. `package.py` also copies the archives into `web/public/` so the console serves them; the web build and live downloads have been checked against the source archives.

The full browser steps are in the [English illustrated walkthrough](../../docs/PolyGate_Student_Walkthrough_EN.md) and [Chinese illustrated walkthrough](../../docs/PolyGate_学生全流程图文指南_ZH.md).

| ZIP | Build method | Port | What to test |
| --- | --- | ---: | --- |
| [`mock-agent-python.zip`](dist/mock-agent-python.zip) | Python template | 8000 | `GET /`, `GET /health`, `POST /api/chat` with `{"message":"hello"}` |
| [`mock-counter-node.zip`](dist/mock-counter-node.zip) | Node.js template | 3000 | `GET /`, `GET /health`, `GET /api/count` |
| [`mock-inspector-dockerfile.zip`](dist/mock-inspector-dockerfile.zip) | Use my Dockerfile | 8080 | `GET /`, `GET /health`, `GET /api/request` |

In PolyGate, open **Source ZIP**, choose the matching build method, upload the ZIP, and select **Queue build**. After the build and image scan show success, choose **Create and deploy app** and enter the port above. For the HTTP health path, use `/health`.

The Python sample returns a deterministic mock agent reply. The Node sample has an in-memory counter that resets when its container restarts. The Dockerfile sample displays a harmless request summary. None of these samples is a real model agent.

All three archives were built and deployed on the internal pilot on 9 November 2026. Their home pages and `/health` endpoints returned HTTP 200 through both worker entry addresses. The temporary test apps were deleted after verification.

Regenerate the ZIPs after editing source files:

```powershell
python examples/polygate-mock-projects/package.py
```
