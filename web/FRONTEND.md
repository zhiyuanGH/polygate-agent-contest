# PolyGate 前端开发文档

本文面向接手学生控制台的开发者。学生页面和交互使用英文；本文使用中文说明代码结构与修改方法。学生操作步骤见 `docs/` 下的中英文指南。

## 技术栈与入口

- React 19、TypeScript 5、Vite 7、Ant Design 5、TanStack Query 5、React Router 7；依赖版本以 `package.json` 和锁文件为准。
- `src/main.tsx` 注册 Ant Design 主题、QueryClient、BrowserRouter 和全局样式。主色 `#8f1329` 取自 PolyU 主题。`src/i18n.ts` 维护现有英文文案。
- `src/App.tsx` 定义页面外壳、路由及多数学生功能；`src/ImagePage.tsx`、`ActivityPage.tsx`、`HelpPage.tsx`、`AdminAccountsPage.tsx`、`AdminResourcesPage.tsx` 分离了较大的页面。
- `src/api.ts` 集中定义 API 类型与请求函数。页面应通过这里调用 `/api/v1`，避免自行拼接请求或重复处理错误。
- `src/styles.css` 负责视口自适应、侧栏、内容区域和响应式布局。桌面页面外框占满浏览器视口，内容较长时在内容区内滚动。

## 页面与主要动线

| 路由 | 页面 | 主要操作 |
| --- | --- | --- |
| `/` | Overview | 配额、应用列表、创建后部署、更新镜像、调整资源/端口、停止、查看逐 Pod 日志、历史日志、事件与资源指标 |
| `/apps/new` | New app | 创建应用草稿；名称在同一账号内唯一，端口填容器实际监听端口 |
| `/images` | My images | 提交镜像引用、上传 `docker save` 的 `.tar`、查看扫描状态、查看或下载完整 Trivy JSON 报告 |
| `/source` | Source ZIP | 上传源码 ZIP、查看构建进度与日志、使用构建完成的镜像 |
| `/secrets`、`/volumes` | Secrets、Volumes | 在后端开放能力时管理应用环境变量与存储 |
| `/activity`、`/help` | Activity、Help | 操作记录与英文使用提示 |
| `/admin/accounts`、`/admin/cluster` | 管理员页面 | 账号和资源管理；仅管理员在导航中看到 |

应用访问地址依据应用的稳定 ID 生成，不直接由应用名称推导。前端从 `/api/v1/me` 的 `appAccessBaseURL` 与 `appAccessRouting` 获取访问地址信息：`path` 模式为 `https://apps.polylink.org/<app-id>/`，省略或 `host` 模式保留旧 `<app-id>.apps.<worker>.sslip.io:30444/`。前端只负责生成链接，路径剥离、重定向与 Cookie 改写由 App Gateway 完成。学生作品应使用相对资源与 API 路径，或显式配置 `/<app-id>/` 基路径；绝对根路径资源不会自动修正。同一公开域名下的作品共享浏览器 origin，路径模式仅用于路由，不提供浏览器存储隔离。应用部署前要先等镜像状态变为 `Approved`；试点扫描策略允许 High，Critical/Unknown 仍阻止部署。

## 数据、认证与错误处理

`api.ts` 的 `request` 函数使用同源 Cookie；写操作从 `/auth/csrf` 取令牌并附加 `X-CSRF-Token`。写操作的幂等键由 API 函数管理。对外展示错误时优先使用 API 返回的错误码和消息；不要把整个原始响应塞进页面。登录跳转 `/auth/login`，退出先调用 `/auth/logout` 清理平台会话，再跳到 `/auth/oidc-logout` 清理身份提供方会话。

页面使用 TanStack Query 保存服务端状态。`me`、`apps`、`operations` 等查询按页面指定的间隔轮询；变更成功后通过 `invalidateQueries` 刷新相关数据。添加新操作时，核对其成功回调是否刷新配额、应用列表、操作记录或镜像列表。`src/main.tsx` 的默认查询设置是失败重试一次，窗口重新聚焦时不自动刷新。

ZIP 与镜像 tar 均先创建上传会话，再按后端返回的分片大小上传，最后调用 `complete`；当前最大分片为 8 MiB。ZIP 页面限制压缩包 100 MiB，镜像 tar 页面限制 1 GiB。进度条表示客户端已发送的分片，100% 后服务端导入或构建仍可能继续。`My images` 的报告入口调用 `GET /api/v1/images/{id}/scan-report`，下载入口加 `?download=1`；后端检查报告所有者，前端不应缓存报告到浏览器持久存储。

Overview 的 Diagnostics 弹窗先用 `GET /api/v1/apps/{id}/pods` 列出当前 Pod，再以可选 `?pod=` 调用 `/logs`。Pod 列表和当前日志每 5 秒刷新；所选 Pod 若有等待、Pod 状态或上次容器退出原因，会显示在当前日志上方（含上次退出码）。这些状态来自 Kubernetes，不保证已删除 Pod 仍有状态可读；无当前 Pod 时提示查看历史日志与事件。当前日志由 Kubernetes 取最近 200 行，API 从中保留末尾最多 128 KiB，页面将最新行放在顶部；读入最多 4 MiB，超限会明确报错。历史日志走 `/logs/history?hours=24|168&before=<纳秒时间戳>&pod=<Pod 名>`，按新到旧展示 Pod 名和时间；每页最多 200 行、128 KiB，页面可继续加载更早的记录。`Historical Pod` 的候选项来自当前 Pod 和已加载的历史页；选中后由后端按账号、App 和精确 Pod 名筛选，切换条件会重置翻页游标。该选择器不会枚举尚未加载的历史 Pod，也不是完整日志导出。源码 ZIP 的 `View log` 分块读取 `/source/builds/{id}/log?offset=...`，弹窗将最新一行放在顶部并仅保留最新 2 MiB；已清理的构建日志会显示未保留。镜像本身没有运行日志，`My images` 展示扫描状态、已完成扫描的 Trivy 漏洞报告，以及 `Failed` 扫描的错误码、重试次数、最后检查时间和可下载的失败摘要。失败摘要来自持久扫描任务，不包含扫描器原始日志；运行容器日志在 Overview 中查看。

构建日志弹窗将最新一行放在顶部，默认跟随最新输出；用户向下滚动会暂停跟随，回到顶部或重新打开 **Follow latest output** 可恢复跟随。浏览器测试使用真实完成的构建日志验证打开、暂停和恢复，不修改构建任务。

当前容器日志支持可选 `?previous=1`。后端先按账号和 App 校验 Pod 归属，再核对 `app` 容器至少重启一次，才向 Kubernetes 请求上一次容器日志；没有前一次输出时返回 `PREVIOUS_LOGS_NOT_FOUND`（404）。Diagnostics 只有在所选 Pod 有重启记录时才启用 **Previous attempt**，切换会使用独立的查询缓存键。已删除 Pod 的旧输出仍从 Historical container logs 查询。

## 本地开发和验证

在仓库根目录运行：

```bash
cd web
npm ci
npm run dev
```

`npm run dev` 启动回环地址上的开发 API 和 Vite 前端；Vite 将 `/api`、`/auth` 代理到本机 `127.0.0.1:8080`。浏览器打开 `http://127.0.0.1:5173/`。本机模式用于开发和测试，不作为生产环境。改动后至少执行 `npm run build`（含 TypeScript 检查）；涉及认证、上传、部署或报告时，再运行对应的浏览器烟雾测试脚本，核对网络请求与页面状态。部署流程、脚本和生产检查见根目录 `DEVELOPMENT.md` / `DEVELOPMENT.zh-CN.md`。

## 修改位置速查

- 增加路由：在 `src/App.tsx` 的 `Routes` 和 `Shell` 导航同时添加；管理员能力依照 `/me` 的角色和能力字段控制。
- 增加 API：先在 `src/api.ts` 加类型与函数，再在页面里使用 Query/Mutation；完成后刷新相关查询。
- 调整视觉：主题 token 在 `src/main.tsx`，全局布局在 `src/styles.css`，局部样式优先使用现有类名。
- 修改报告展示：`src/ImagePage.tsx` 负责扫描摘要、报告弹窗、JSON 下载；扫描决策规则在 Go 后端 `internal/harbor/scan.go`，不能只改前端提示。
- 修改学生帮助：更新 `src/HelpPage.tsx`，同步检查 `web/public/` 中的在线指南与 `docs/` 中的 Markdown 指南。

当前控制台没有完整组件测试套件，主要依靠 TypeScript 构建、后端测试、浏览器烟雾测试和上线后的发布检查。`App.tsx` 仍包含多个页面和较多操作状态；新增复杂页面时建议抽出独立文件，保持 API 和路由行为清晰。
