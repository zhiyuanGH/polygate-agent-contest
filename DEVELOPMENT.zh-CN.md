# PolyGate 开发交接文档

最近核对：2026-11-09。本文供新开发者和接手的 Agent 使用。开始工作前，先检查当前代码、集群状态和最近的验收记录；较早的设计方案与实施记录不能替代现场核对。[English](DEVELOPMENT.md)。

## 1. 当前产品与交付边界

PolyGate 是供学生提交和运行容器作品的英文控制台。学生可以提交公开镜像引用、上传 `docker save` 的 `.tar`，或上传源码 ZIP 构建镜像；查看构建日志、Trivy 扫描结果，部署已批准的镜像，访问作品 URL，并管理日志、Secret、卷、更新、回退、重启、停止和删除。管理员可以管理账号与额度。身份认证使用本地 Keycloak 账号，暂不接学校 SSO。

当前是**邀请制、提交内容视为可信的实验室内网试点**。源码 Dockerfile 在共享构建宿主上执行，不能把它视作恶意代码隔离环境。本机仅用于开发和验收，不承载生产流量。正式域名与受信任证书、外部告警接收端、恢复密钥独立副本、第二存储副本和不可信构建隔离仍是后续交付项。

## 2. 代码从哪里看起

| 目录或文件 | 作用 |
| --- | --- |
| `web/src/App.tsx`、`ImagePage.tsx`、`HelpPage.tsx` | 页面、应用表单、镜像页和在线帮助。 |
| `web/src/api.ts` | 浏览器 API 客户端，包含 ZIP 和镜像 tar 的分块上传。 |
| `cmd/api`、`internal/server` | Go HTTP API；路由总表在 `internal/server/server.go`，这里做账号归属与请求校验。 |
| `internal/auth` | Keycloak OIDC、服务端会话、退出登录和 CSRF。 |
| `internal/postgres`、`db/migrations` | 事务、配额、任务租约、审计和数据库迁移。 |
| `internal/source`、`internal/sourcebuild`、`cmd/sourcebuildworker` | ZIP 检查、源码构建任务和 worker。 |
| `internal/registry`、`internal/harbor`、`internal/imagescan`、`cmd/imagescanworker` | 镜像解析、Harbor 私有项目、Trivy 扫描与证据。 |
| `internal/deploy`、`internal/kubeobjects`、`cmd/worker` | Kubernetes 部署、状态对账、停止与资源清理。 |
| `internal/appgateway`、`cmd/appgateway` | 按应用 UUID 解析作品域名并转发到对应 Service。 |
| `examples/polygate-mock-projects` | 三个可上传的示例项目。 |
| `deploy/`、`infra/dev/` | 实验室部署清单与运维脚本；公开仓库只导出其中允许公开的部分。 |

当前后端路由使用 Go `net/http`，前端使用 React、TypeScript、Vite、Ant Design 和 TanStack Query。数据库、身份系统、构建后端及 Kubernetes 组件均在服务器运行。部分早期技术方案描述的是目标架构，不能当作已上线状态。

## 3. 本地运行与测试

需要 Go **1.27**、Node.js **22+** 和 npm。在仓库根目录运行：

```bash
go test ./...
go vet ./...
cd web
npm ci
npm run build
npm run dev
```

`npm run dev` 启动仅监听本机回环地址的内存 API 和 Vite 页面，浏览器打开 `http://127.0.0.1:5173/`。Vite 把 `/api` 与 `/auth` 代理到本机 8080 端口。这个模式适合前端和接口形状开发，**不连接真实 Keycloak、PostgreSQL、Harbor，也不构建或部署应用**。用 Ctrl+C 停止。需要完整认证环境时，实验室工作区有 `infra/dev/` 的本地 PostgreSQL/Keycloak 脚本；凭据保存在仓库之外。

`go test ./...` 在没有测试数据库地址时可能跳过数据库集成测试。改动事务或迁移时，必须用独立的临时数据库设置 `POLYGATE_TEST_DATABASE_URL` 或测试专用 URL，补跑相关集成测试；不能把生产库作为测试库。

## 4. 三条主要业务动线

1. **源码 ZIP**：前端按 8 MiB 分块上传；`internal/source` 检查路径、符号链接和大小（压缩后最多 100 MiB、展开后最多 1 GiB）；PostgreSQL 保存持久任务；`cmd/sourcebuildworker` 调用可信构建后端，镜像推入 Harbor。Source ZIP 页面显示任务状态和日志。
2. **已构建镜像**：公开引用由 registry resolver 解析；本机 `.tar` 由镜像上传接口接收，再由 `internal/harbor/import_tar.go` 推入该账号的 Harbor 项目。tar 最多 1 GiB，须包含一张 `linux/amd64` 镜像。上传成功并不等于允许部署。
3. **扫描与部署**：`cmd/imagescanworker` 运行 Harbor Trivy，记录固定镜像摘要、扫描 ID、报告哈希、严重度和自动决策。试点策略 `severity-v2` 允许含 High 的镜像，阻止 Critical、Unknown 和未识别的严重度；High 仍显示在报告中。已完成的扫描报告可在 **My images** 查看和下载。新报告连同 SHA-256 校验值存入 PostgreSQL；旧报告首次从 Harbor 读取后归档。批准后的镜像由数据库事务核对账号与全局容量并预留资源，再由 `cmd/worker` 对账 Kubernetes 对象，最终显示 Running 或 Failed。

前端结构和开发流程见 [web/FRONTEND.md](web/FRONTEND.md)。

修改写接口时要保留账号归属检查、CSRF、幂等键与任务重试语义。新增 Kubernetes 对象必须带账号 ID、应用 ID 标签，供网关和清理逻辑核验。对失败路径要检查是否留下任务租约、容量预留或孤儿资源。

## 5. 应用名称、URL、端口与资源

学生填写的 **App name** 是页面上的展示名：1–40 位小写字母、数字或连字符，首字符为字母，不允许连续 `--`；同账号内不能重名，不同账号可以同名。每次新建应用还会生成独立 UUID。作品 URL 采用 `https://<app-id>.<配置的作品域名>/`，**不使用展示名或学生用户名**。Kubernetes 资源名为 `app-` 加去掉连字符的 UUID。

**App port 是容器内 HTTP 程序真正监听的端口**，不是浏览器 HTTPS 端口或宿主机端口。程序应监听 `0.0.0.0`；仅监听 `127.0.0.1` 会导致平台无法访问。三个示例分别是 Python `8000`、Node.js `3000`、Dockerfile `8080`，自己的程序要看启动命令或日志。Dockerfile 的 `EXPOSE` 只是声明。健康路径只有在健康时返回 HTTP 2xx/3xx 才填写，否则留空使用 TCP 检查。若应用因端口错误失败，可在 Overview 用 **Edit port** 修改，再用同一张批准镜像重新部署。

单容器内存上限为 **4096 MiB（4 GiB）**；另有账号额度和全局 **100 GiB / 60 Pod** 预算。草稿和已停止应用仍占用账号的已配置额度。改额度逻辑先看 `internal/domain/resources.go` 及数据库准入事务。

## 6. 中英学生指南怎样更新

指南源文件是 `docs/PolyGate_Student_Walkthrough_EN.md`、`docs/PolyGate_学生全流程图文指南_ZH.md`，简版见 `docs/PolyGate_Student_Quick_Start.md`。实验室工作区执行 `python infra/dev/package-student-guides.py`，把 Markdown 与 20 张步骤截图生成 `web/public/student-guide-en.html` 和 `student-guide-zh.html`。修改学生流程时，也检查 `web/src/HelpPage.tsx` 与应用表单。重新构建 Web 后，用浏览器核对两种语言、图片、链接和下载；不要只改生成的 HTML。

公开仓库导出的指南 Markdown 把实验室入口改为 `YOUR-POLYGATE-HOST`，图片引用指向公开的 `web/public/guide-*.png`。实验室发布时以私有工作区中的 Markdown 为源，重新生成页面。

## 7. 发布与回归顺序

1. 跑 Go 测试和 vet、前端构建；涉及数据库的改动另跑一次临时库集成测试。改动学生动线时，用真实浏览器验收成功与失败路径。
2. 数据库迁移前先制作并验证备份；先迁移，再发布依赖新 schema 的 worker/API，最后发布 Web。实验室工作区有 `infra/dev/migrate-platform-db.py`、`build-deploy-api.py`、`build-deploy-web.py` 和各 worker 发布脚本。公开仓库不包含生产凭据与部署自动化。
3. 发布后运行 `python infra/dev/verify-v01-release.py`。它检查自然健康 Job、集群外入口探测、节点、账号、队列与 HTTPS/OIDC；这只是快速门槛，不能代替所改功能的浏览器端到端验收。
4. 核对 API/worker 日志及临时账号、应用、Job、容量预留是否清理。把改动、测试、现场结果和回退办法写进带日期的实验室实施记录，检查公开导出没有凭据后再同步仓库。

**不要提交**密码、kubeconfig、私钥、Harbor 凭据、备份、学生源码包或生产日志。测试账号密码通过实验室私有交付渠道传递。

## 8. 下一批工作与已知局限

- 正式域名、可信 TLS 证书、OIDC issuer/回调/退出地址及作品域名切换，随后进行校外浏览器验收。
- 集群外告警接收端与故障/恢复通知验收。当前探测会记日志，但不能无人值守通知值班人员。
- 恢复解密私钥的独立副本与实际恢复演练；有合适磁盘后补存储第二在线副本。
- 在开放未知或恶意 Dockerfile 前，完成宿主维护和隔离构建环境；当前可信试点不覆盖此威胁模型。
- 对约 50 名学生完成正式验收矩阵和开放决策；现有并发与 4 GiB 压测仅覆盖已执行的场景。
- 扫描报告已随扫描证据写入 PostgreSQL；第 27 版迁移后的 19 份旧报告已回填并逐份核对哈希。发布门槛 `verify-image-scan-archive.py` 检查有效报告是否齐全、归档摘要及批准镜像的策略版本。2026-11-09 的逻辑备份已在临时数据库恢复，24 份有效报告全部存在且摘要错误为 0；生产数据库的真实故障切换仍未演练。

实验室的最新现场入口为工作区 `README.md`、`docs/PolyGate_下一阶段计划_集群试点到参赛开放.md`、最近的带日期验收记录和实时发布检查。历史记录只证明当时的状态，接手时仍要复核现场。
