# PolyGate 协作开发与持续部署

> Public copy: production host identifiers are omitted. Operators should use the private deployment repository as the source of truth.

更新：2026-11-10。本文给接手开发和运维的成员使用。

## 当前交付链

```text
开发者分支 → 公共源码仓库 PR → Go/Web/镜像构建检查 → 合并 main
  → GitHub Actions 发布 linux/amd64 Web 镜像到 GHCR
  → 私有部署仓库每 10 分钟查询最新 main 的镜像摘要并提交
  → 集群 Flux 每 1 分钟读取私有仓库 → Web Deployment 滚动更新
```

| 用途 | 地址 |
| --- | --- |
| 应用源码与协作 PR | https://github.com/KasperXMS/polygate-agent-contest |
| 生产部署状态（需仓库权限） | https://github.com/KasperXMS/polygate-deploy |
| 镜像 | `ghcr.io/kasperxms/polygate-web@sha256:<digest>` |
| 生产网页 | https://polygate.polylink.org/ |

集群从私有仓库主动拉取配置。GitHub Actions 使用托管运行器，不依赖开发者电脑，也不在实验室主机上运行公共仓库的 PR 代码。集群的部署密钥只有私有仓库读取权限。现有平台 API、网关、数据库、Keycloak、Harbor、学生作品和 Tunnel 暂未纳入 Flux；它们的发布流程仍按原运维方式执行。不要把本文的 Web 自动部署流程误用于这些组件。

## 开发者如何提交

1. 获得公共源码仓库的协作权限，创建自己的分支；不要直接推送 `main`。
2. 本地执行 `go test ./...`。改动前端时执行 `cd web && npm ci && npm run build`。
3. 推送分支并发起 PR，说明改动、测试结果和界面变化。代码与文档请放在公共源码仓库；生产主机地址、账号数据、密钥与备份不得提交。
4. 等待 PR 的 `go`、`web`、`build` 三项检查通过，解决评论后合并。公共仓库 `main` 已设置 PR 与检查门禁。
5. 合并后查看公共仓库 **Actions → Web image**。成功时工作流摘要会给出完整镜像摘要。
6. 查看私有仓库 **Actions → Promote public Web image**。定时任务每 10 分钟检查一次；需要马上推进时，可在该工作流点击 **Run workflow**。它仅在对应的 GHCR 镜像发布后更新部署文件。
7. 等待 Flux 同步，检查生产网页和改动涉及的功能。

GitHub 定时任务可能延迟，因此“每 10 分钟”是检查频率，不是交付时限。生产镜像以摘要固定；不要使用 `latest` 或直接改服务器上的镜像标签作为常规发布方式。

## 运维如何检查一次发布

在有集群管理权限的机器上执行：

```bash
kubectl -n flux-system get gitrepository,kustomization,pods
kubectl -n polygate-system rollout status deployment/polygate-web --timeout=300s
kubectl -n polygate-system get deployment polygate-web -o jsonpath='{.spec.template.spec.containers[0].image}'
kubectl -n polygate-system get pods -l app=polygate-web -o wide
curl -fsSI https://polygate.polylink.org/
```

`GitRepository` 和 `Kustomization` 应为 `Ready=True`。Web Deployment 应有 2 个就绪副本。两个工作节点 `worker-a` 与 `worker-b` 都已验证能读取 GHCR，但首次下载镜像可能比较慢。Deployment 使用 `maxUnavailable: 0`，旧副本会保留到新副本就绪。

仓库中的控制文件：

| 文件 | 作用 |
| --- | --- |
| `clusters/production/polygate-web/deployment.yaml` | Web 的生产镜像摘要和 Deployment 配置 |
| `clusters/production/polygate-web/promotion-mode` | `auto` 自动晋升；其他值暂停晋升 |
| `.github/workflows/promote-web.yml` | 查询公共 `main` 的镜像摘要并提交到本仓库 |
| `clusters/production/flux-system/gotk-sync.yaml` | Flux 的私有仓库源与同步设置 |
| `clusters/production/flux-system/gotk-components.yaml` | 已安装的 Flux 控制器清单 |

## 暂停发布与回退

发现 Web 新版本有问题时，先在私有部署仓库把 `promotion-mode` 从 `auto` 改成 `paused` 并提交，以免定时任务再次推进最新镜像。随后把 `deployment.yaml` 的 `image` 改成上一个已验证的 `ghcr.io/kasperxms/polygate-web@sha256:<digest>`，提交并等待 Flux 同步。可通过 `git log -p -- clusters/production/polygate-web/deployment.yaml` 找上一版本摘要。按上面的检查命令验证两副本和网页。

如果 Git 或 Flux 同步本身不可用，先暂停 Flux，再用 Kubernetes 的 Deployment 历史回退：

```bash
kubectl -n flux-system patch kustomization flux-system --type merge -p '{"spec":{"suspend":true}}'
kubectl -n polygate-system rollout undo deployment/polygate-web
kubectl -n polygate-system rollout status deployment/polygate-web --timeout=300s
```

修好私有仓库中的目标版本后，用 `kubectl -n flux-system patch kustomization flux-system --type merge -p '{"spec":{"suspend":false}}'` 恢复同步。首次 GHCR 切换前的本地镜像是 `<previous-local-image>`，其拉取策略为 `Never`；历史回退会同时恢复该策略。不要只执行 `kubectl set image` 而不暂停或修正 Git 中的目标版本，否则 Flux 会改回去。

## 权限与当前边界

公共源码仓库的 `main` 要求 PR 和 `go`、`web`、`build` 检查通过；现在没有强制人工批准人数，等第二位维护者加入后可以启用至少一人审查。私有部署仓库受当前 GitHub 套餐限制，无法开启私有仓库分支保护；只授予实际运维人员写入权限，生产变更使用 PR 交流和复核。私有仓库禁止提交密码、kubeconfig、SSH 私钥、学生账号表或数据库备份。

自动化目前仅覆盖 **PolyGate Web 前端**。API、构建 worker、网关、Keycloak 和学生容器不会随源码 `main` 自动更新。扩展到这些组件前，需要为各组件补齐镜像构建、健康检查、不可变摘要、部署清单及回退验证。

## 首次上线验收记录（2026-11-10）

- 公共源码 `main` 的 Web 镜像构建成功，提交为 `51f6f05a2a964e2b02ea30ff079c73d5099eae11`，镜像摘要为 `sha256:c01ae691b4467f14c25142e407e3b72c07585077b200e3d40e397c268fe462d8`。
- `worker-a` 和 `worker-b` 均成功拉取该摘要；生产 Web 在两台节点各运行一个就绪副本，滚动更新完成。
- `https://polygate.polylink.org/` 返回 200，前端 JS 资源返回 200；Keycloak 的公开配置返回 200，未登录访问 `/api/v1/me` 返回预期的 401。
- Flux 的 `GitRepository` 与 `Kustomization` 均为 `Ready=True`，已应用私有仓库提交 `e950437a69c061e2d8537c9747f3ceafeb8792cd`。私有仓库的手动晋升工作流运行成功；同一摘要重复执行时不产生部署提交。
- 首次安装时 Flux 控制器镜像下载较慢，因此 Web 使用仓库内同一清单先行滚动更新；Flux 启动后已接管相同的目标状态。后续发布由自动晋升和 Flux 完成。
