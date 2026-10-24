# Go 版部署、授权与迁移

设计基线：`ab2c186`，实施计划：[Go 重写计划](go-rewrite-plan.zh-CN.md)。原 TypeScript [部署文档](https://github.com/dszp/mstodo-mcp-cloudflare/blob/main/DEPLOYMENT.md) 继续用于原服务。

## 1. Docker Compose

当前 Go 模块位于仓库根目录，以下 Compose 命令均在根目录执行。默认拉取 GHCR 成品镜像，无须安装 Go 或在部署主机编译。需要 Docker Engine、Compose 和本地文件系统数据目录，只运行一个实例，不共享到 NFS 或多副本。镜像使用同一标签提供 linux/amd64 和 linux/arm64；Docker 自动选择对应架构。

```bash
cp .env.example .env
mkdir -p data
sudo chown 10001:10001 data
sudo chmod 700 data
```

修改 `.env`：

```dotenv
PUBLIC_BASE_URL=https://todo.example.com
MSTODO_IMAGE=ghcr.io/qaq112233/mstodo-go:latest
BIND_ADDR=127.0.0.1
PORT=8080
DATA_PATH=./data
LOG_LEVEL=info
```

`PUBLIC_BASE_URL` 必填且只包含 origin，不带业务秘密路径或结尾的子路径。应用使用它生成微软回调，不信任 `X-Forwarded-*`。本地开发可使用 `http://localhost:8080` 等环回地址；公网必须使用 HTTPS。

容器内监听 `:8080`，数据库固定在 `/data/mstodo.db`；这些部署值由 Compose 提供。时区、微软应用信息和业务访问模式由网页保存到 SQLite。初始目录所有者必须允许容器 UID/GID `10001:10001` 写入，数据库以及 WAL/SHM 都在同一目录。

```bash
docker compose pull
docker compose up -d
docker compose logs mstodo
curl --fail-with-body http://127.0.0.1:8080/health
```

首次日志给出 `/admin/<secret>`，请收藏。之后获取链接使用 `docker compose exec mstodo mstodo admin-url`。此命令输出敏感信息，不应粘贴到工单或公开日志。管理员链接、Bearer token、秘密路径和微软 token 是不同凭据。

## 2. 公网 HTTPS

用现有反向代理将 `https://todo.example.com` 转发到 `127.0.0.1:8080`，或者由 Tunnel 转发。保留原始请求方法、查询串、Authorization 和 MCP 相关请求头；上游读取超时至少 75 秒。默认 Compose 端口只监听宿主机环回地址；代理在另一个容器时按实际网络调整映射，避免无意暴露整个局域网。

HTTPS 保护传输，访问验证决定谁能读写。若使用 Cloudflare Access，网页访问和机器调用分别按自己的策略设置；启用 Service Token 时，墨水屏和 MCP 端也需要它要求的凭据。应用不内置 Access JWT 验证，也不信任自报身份的代理 header。反向代理日志应屏蔽 `/admin/`、`/s/` 的真实路径和授权回调查询串，应用日志只记录业务操作名。

## 3. 注册微软应用

在 Microsoft Entra 管理中心进入 **App registrations / 应用注册**，新建或复用应用，选择适合个人账号或组织账号的支持类型。记录 Application (client) ID；tenant 可用租户 ID，个人账号按应用支持范围使用 `common` 或 `consumers`。

在 **Authentication → Add a platform → Web** 添加：

```text
https://todo.example.com/auth/microsoft/callback
```

这是固定回调，不含 `/s/<secret>` 或管理员路径。更改公网域名后，同时修改 `PUBLIC_BASE_URL` 和微软应用登记的回调。创建 client secret 并保存它的 **Value**，不能填 Secret ID；过期后在设置页更新并重新授权。

添加 Microsoft Graph **Delegated / 委托**权限：

- `Tasks.ReadWrite`
- `User.Read`
- `offline_access`

My Day、无损移动、任务和步骤手动排序还需要 **Office 365 Exchange Online → Delegated → Tasks.ReadWrite**。若界面无法找到，可在应用 Manifest 的 `requiredResourceAccess` 中加入以下资源项；保留已有 Graph 等其他条目：

```json
{
  "resourceAppId": "00000002-0000-0ff1-ce00-000000000000",
  "resourceAccess": [
    {"id": "6b49b74d-642f-4417-a6b4-820576845707", "type": "Scope"}
  ]
}
```

这是基线已验证的委托权限，不是邮件、日历或文件权限。租户要求时执行管理员同意。授权流程使用授权码、PKCE S256 和应用 secret；Graph 与 Exchange 的 access token 分开，通过一个持久化 refresh token 串行刷新。

微软流程参考：[授权码流程](https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-auth-code-flow)。内部接口的历史验证见 [My Day 记录](myday-feed-verification.zh-CN.md)，其中的真实成功仅属于 TypeScript 基线。

## 4. 首次授权与检查

1. 打开管理员链接，填写 tenant、client ID、client secret 和**预期微软账号邮箱**；确认时区（默认 `Asia/Shanghai`）。
2. 保存设置。更改应用信息或预期账号会清除旧授权和可重建缓存。
3. 点击“连接 / 重新授权微软”，使用预期账号登录。服务通过真实 Graph `/me` 验证邮箱；不匹配时拒绝保存 token。授权页面与回调应在同一个浏览器完成，以保留一次性流程 cookie。
4. 回到设置页，点击“分别测试 Graph 和 My Day”。Graph 可用不代表 Exchange 权限可用；My Day 权限不足时检查 Manifest、同意状态并重新授权。
5. 展开地址与凭据，设置墨水屏和支持手动 header/URL 的 MCP 客户端。默认 Bearer；`path` 使用秘密前缀，`none` 表示主动开放或依赖外层验证。
6. 核对 My Day 任务 ID 集合、完成状态及顺序。先只读比对，再在独立测试清单中验收写入。

设置页访问和 `/health` 都不会自动拉任务。按钮测试会读清单目录和完整 My Day；七天 token maintenance 仅维护授权，临时失败冷却 24 小时，不触发任务同步。

## 5. 数据、升级、备份和恢复

数据目录含应用 secret、refresh token 和私人任务。限制宿主机访问，不上传 Git；首版依赖目录权限保护，未增加应用层加密。

先停止容器，再备份**整个**数据目录，避免只复制 WAL 模式下的主数据库：

```bash
docker compose stop
sudo tar -czf mstodo-backup.tar.gz data
docker compose start
```

若 `DATA_PATH` 不为 `./data`，相应修改备份路径。备份应存放在受保护位置。恢复时停止容器，将备份还原到挂载目录并恢复 `10001:10001` 所有权，再启动。授权及访问凭据随数据库保留；微软撤销或 token 已失效时仍需重新授权。

升级时执行 `docker compose pull` 和 `docker compose up -d`。SQLite 使用顺序迁移和 `user_version`；更旧的可执行文件遇到较新数据库会拒绝打开。回退前保留与旧版本匹配的完整备份。无需为重建容器轮换业务凭据或重新授权。

## 6. 并行验收和切换

Go 用独立地址、独立数据目录和独立授权，不导入旧 Cloudflare KV、Durable Object 或 MCP OAuth grants。保留原 TypeScript 服务，先对同一日期做只读比对；再使用专用测试清单验收任务、提醒、重复规则、步骤、链接、移动新 ID 和网页顺序。已有附件随原任务移动，但附件完整性需真实样本核验。

验收后切换墨水屏和 MCP 地址；回退时恢复原地址。旧同步和订阅应在验收之后才停用。公网 HTTPS、墨水屏、Cloudflare Access 和最终 MCP 客户端均为独立验收项；不要将本地模拟测试写成这些已通过。
