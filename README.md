# mstodo-go

[![Build and publish](https://github.com/qaq112233/mstodo-go/actions/workflows/build.yml/badge.svg)](https://github.com/qaq112233/mstodo-go/actions/workflows/build.yml)

轻量的 Microsoft To Do 个人服务：一个 Go 进程、一个 SQLite 文件，同时提供 REST、MCP 和初始化网页。主要入口是墨水屏使用的 `GET /api/v1/myday`，完整读取“我的一天”，默认保留已完成任务。

本项目参考并重写自 **[dszp/mstodo-mcp-cloudflare](https://github.com/dszp/mstodo-mcp-cloudflare)**，感谢原作者 David Szpunar。微软请求、Substrate 移动及排序行为参考原实现，并结合 My Day 直接 feed 的验证结果重写。设计基线为拆分前的 `ab2c186`。沿用 [MIT 许可](LICENSE)，保留原作者版权声明。

这是独立 Go 仓库，源码位于根目录，不包含旧版 TypeScript、Cloudflare 配置、旧版 Git 历史或运行数据。所有微软授权和设置都由当前服务独立保存。

## 功能

- My Day 直接 feed、完整分页、显式本地日期、已完成任务及稳定排序。
- 清单与任务 CRUD、完成/恢复、正文、重要性、日期、提醒、重复规则和分类字段。
- 步骤、关联链接、中文包含搜索、跨清单查询、无损移动及三类手动排序。
- 32 个 MCP 工具，使用官方 Go SDK 的 Streamable HTTP，与 REST 共用业务逻辑和缓存。
- 设置网页、独立管理员链接、Bearer / 秘密路径 / 无验证三种业务访问模式。
- 180 秒按需缓存；空闲时不读取任务，仅低频维护微软 token。

附件上传下载保留为后续待办。My Day、移动和排序依赖微软内部 Substrate 接口，可能随微软调整。

## Docker Compose 启动

下载或克隆本仓库，在仓库根目录执行：

```bash
cp .env.example .env
# 编辑 .env：PUBLIC_BASE_URL=https://todo.example.com
mkdir -p data
sudo chown 10001:10001 data
sudo chmod 700 data
docker compose pull
docker compose up -d
docker compose logs mstodo
```

默认镜像为 **`ghcr.io/qaq112233/mstodo-go:latest`**，同一标签包含 **`linux/amd64`（x64）与 `linux/arm64`**，Docker 自动选择宿主机对应版本。默认只将服务映射到宿主机 `127.0.0.1:8080`，由你的 HTTPS 代理或 Tunnel 转发。

首次日志提供随机管理员链接，请妥善保存。遗失时主动执行：

```bash
docker compose exec mstodo mstodo admin-url
```

在设置页保存微软应用信息和预期账号，然后连接微软。固定回调为 `PUBLIC_BASE_URL/auth/microsoft/callback`。Graph 和 Exchange 委托权限的注册步骤见 [部署教程](docs/go-setup.zh-CN.md)。默认生成的业务 Bearer 与管理员链接、微软 token 相互独立。

数据库和授权保存在 `./data`，容器重建保留数据。数据目录含 secret 和私人任务，请勿公开。备份时先停止容器，再备份整个目录。

My Day 失败会返回明确错误，不把过期任务或部分分页包装为成功；墨水屏应保留上次成功画面。

## 文档与验证状态

- [部署、微软注册、权限、备份与升级](docs/go-setup.zh-CN.md)
- [REST API](docs/go-rest-api.zh-CN.md)
- [MCP 地址、工具与客户端要求](docs/go-mcp.zh-CN.md)
- [Go 自动化与真实微软验证](docs/go-validation.zh-CN.md)
- [GitHub Actions 与 GHCR 发布验证](docs/publication.zh-CN.md)

已经完成本机独立微软授权、My Day 与浏览器 feed 比对、专用测试清单真实读写和清理、授权重启保留。生产 HTTPS、墨水屏、最终 MCP 产品客户端和附件样本仍需独立验收。自动化测试、真实账号验证和容器发布分开记录。

## 自动构建与发布

`.github/workflows/build.yml` 提供：

1. PR、main 推送和手动运行执行测试、竞争检测、vet，并生成 Linux amd64 / arm64 静态程序与 SHA-256 校验文件，可从 Actions artifacts 下载。
2. main 推送、`v*` 标签或手动运行，在上述检查完成后构建并推送双架构 GHCR 镜像，使用 `GITHUB_TOKEN`，无需单独保存 Docker 发布密码。
3. main 更新 `latest`；每次发布提供 `sha-<完整提交>`，`v1.2.3` 标签额外提供 `1.2.3` 与 `1.2` 镜像标签。
4. 推送后检查真实镜像索引中的两个架构，并拉取已发布 amd64 镜像运行健康检查。PR 不发布镜像。

Actions 和依赖均锁定版本；首次发布结果及镜像摘要见发布验证文档。要固定部署版本，可修改 `.env` 的 `MSTODO_IMAGE` 为提交标签或 `ghcr.io/qaq112233/mstodo-go@sha256:...`。

下载程序 artifact 后先核对附带的 SHA-256，再执行 `chmod +x mstodo-linux-amd64`（arm64 使用对应文件）。构建包与容器均保留原 MIT 许可。

## 本地开发

使用 `go.mod` 中锁定的 Go 1.27.3，在仓库根目录：

```bash
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -o mstodo ./cmd/mstodo
PUBLIC_BASE_URL=http://localhost:8080 DATA_DIR=./data ./mstodo
```

本地 HTTP 只允许环回地址，公网地址使用 HTTPS。运行不需要 Node、Cloudflare 或外部数据库。SQLite 无 CGo 依赖，网页和时区数据内嵌，Docker 运行层为非 root 的 scratch 镜像。

本地构建镜像：

```bash
docker build -t mstodo-go:local .
MSTODO_IMAGE=mstodo-go:local docker compose up -d --pull never
```
