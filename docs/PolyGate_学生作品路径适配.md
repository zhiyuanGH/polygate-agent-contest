# PolyGate 学生 Web 应用的路径适配

在控制台 **New app** 输入 App name 时，页面会按当前账号实时预览完整公网 URL 和基础路径。该地址在应用部署并达到 Running 后才可访问。`student-a` 与 `student-b` 的真实浏览器预览均已验证；仅填写名称不会创建应用。

更新：2026-11-10。作品的易读地址为 `https://apps.polylink.org/<username>/<app-name>/`，例如 `https://apps.polylink.org/student-a/test-agent-1/`。原来的 `https://apps.polylink.org/<app-id>/` 地址继续可用，适合作为不随展示路径变化的回退链接。

## 为什么不能自动兼容所有应用

网关能从**进入网关的 URL** 中识别作品，并在转发时去掉前缀。它还能改写常见服务端 `Location` 和 `Set-Cookie Path`，并转发 WebSocket、SSE。但若页面中的 JavaScript 发出 `fetch('/api')`，或 HTML 引用 `/static/main.js`，浏览器只会请求域名根路径；请求中已经没有用户名、应用名或 app ID。多个作品共用一个域名，网关无法可靠推断目标作品。把前缀从 UUID 改成易读名称也不会改变这一点。任意压缩后的前端代码、Service Worker、硬编码跨域 URL 更不能靠通用字符串替换保证正确。

## 提交规范

1. 容器内 HTTP 服务监听 `0.0.0.0` 和页面填写的 App port；公网入口始终使用 HTTPS 443，与 App port 无关。
2. 浏览器端静态资源、API、导航和 WebSocket 优先用相对 URL：`assets/main.js`、`api/chat`、`new URL('socket', location.href)`。不要写 `/assets/main.js`、`fetch('/api/chat')` 或 `wss://apps.polylink.org/ws`。
3. 需要绝对公开路径的框架，读取网关传入的 `X-Forwarded-Prefix`，将它用于框架的 base path/root path。该值在易读路由下是 `/<username>/<app-name>`，在 ID 路由下是 `/<app-id>`；旧内网 hostname 路由不提供此前缀。网关会清除客户端伪造的该请求头。
4. 在最终公网地址打开页面，并用浏览器 Network 面板检查资源、API、WebSocket 是否仍请求到了 `/`。只看到容器日志里 `GET /docs 200`，不足以证明页面完整可用。

## FastAPI 示例

FastAPI 的 Swagger UI 会使用 ASGI `root_path` 生成 OpenAPI 地址和 `servers`。对同一镜像同时支持新路径路由和旧 hostname，可在 `create_app()` 中给应用加请求级中间件：

```python
import re
from fastapi import FastAPI, Request

app = FastAPI()
prefix_pattern = re.compile(
    r"^/(?:[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}|"
    r"[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?/[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?)$"
)

@app.middleware("http")
async def poly_gate_prefix(request: Request, call_next):
    prefix = request.headers.get("x-forwarded-prefix", "")
    if prefix_pattern.fullmatch(prefix):
        request.scope["root_path"] = prefix
    return await call_next(request)
```

仅经路径入口部署、不需要旧 hostname 的 FastAPI 应用，也可按[官方反向代理指南](https://fastapi.tiangolo.com/advanced/behind-a-proxy/)在启动时设置固定 `root_path`；但固定值不能同时匹配易读路由、旧 ID 路由和旧 hostname。Workshop 示例的完整改动在 [补丁](patches/workshop-fastapi-public-path.patch)，包括适合 PolyGate UID 65532 与只读根文件系统的 Dockerfile 调整。原始 `zhiyuanGH/PolyGate-Stage-1-Workshop1-Demo` GitHub 仓库未被修改；运行中的 `test-agent-1` 使用从该补丁构建并通过扫描的新版镜像。

该示例原本只提供 API，`/` 返回 404。补丁还添加了相对重定向 `RedirectResponse("docs")`，让页面上的 **Open app** 在易读、ID 和旧 hostname 三种入口都能到达 Swagger 文档。

已在真实浏览器验证 `https://apps.polylink.org/student-a/test-agent-1/docs`：OpenAPI 请求为 `/student-a/test-agent-1/openapi.json`，返回 HTTP 200；Swagger 服务器路径包含相同前缀。见[修复后截图](assets/polygate-workshop-readable-docs-fixed.png)。旧 ID 路由和旧内网 hostname 的 `/docs` 也通过。

## 其他常见应用

| 应用 | 建议 | 当前承诺范围 |
| --- | --- | --- |
| 原生 HTML、简单 Python/Node API | 使用相对资源及 API URL；重定向用本应用路径 | 已验证 HTTP、重定向、Cookie、WebSocket、SSE |
| FastAPI | 用 `root_path` 或上方的请求级适配 | Workshop 示例已做浏览器验收 |
| React/Vite、其他编译前端 | 构建时选择可用的相对资源 base，路由器配置作品 base；逐页检查绝对根路径 | 需逐个项目验证 |
| Streamlit、Gradio | 框架有路径选项，但与本平台“网关剥离前缀”的组合需按具体版本和入口测试 | 暂不承诺透明兼容 |

易读路径帮助开发者预先确定地址，并不能替代应用自身的 base path 支持。需要独立浏览器 origin、根路径或共享 localStorage 的项目，应使用旧内网 hostname 入口或另行评估独立域名方案。
