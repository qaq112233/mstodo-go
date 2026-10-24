# My Day 直接读取接口验证记录

> 以下是拆分前 TypeScript 参考实现的历史证据，不是当前 Go 服务的构建或发布结果。Go 验证见 [验证记录](go-validation.zh-CN.md)。

## 项目目标

本项目最终用于第三方任务看板。Microsoft To Do“我的一天”的任务是核心数据：
应优先保证成员完整、完成状态及时、日期正确和顺序稳定，同时保留现有 MCP、
OAuth、Graph 同步与普通任务能力。

## 2026-10-24 网页只读验证

在已登录的 Microsoft To Do 网页上发现并验证：

```http
GET https://substrate.office.com/todob2/api/v1/myDayFeed/sections/tasks?date=2026-10-24
```

- 网页构建：`4700804 / 2.134.1`。
- 本地时区：`Asia/Hong_Kong`，与项目配置 `Asia/Shanghai` 在本次日期边界一致。
- 显式查询 2026-10-24：HTTP 200，返回 11 个未完成任务，任务 ID 集合与网页
  “我的一天”显示完全一致。
- 用户把其中一个任务设为已完成后重新查询：HTTP 200，仍返回 11 个任务，
  其中 10 个未完成、1 个 `Status=Completed`；该任务的 `CommittedDay` 仍为当天。
- 查询 2026-10-23：HTTP 200，返回 1 个已完成任务。
- 不传 `date`：本次返回 UTC 当天 2026-10-23 的结果，而浏览器本地已是 24 日。
  增加 `Prefer: outlook.timezone="China Standard Time"` 未改变结果。
- 测试 OData `$filter=CommittedDay ge ...` 返回 HTTP 400；不要假定此接口支持
  其他任务集合的全部查询语法。

## 请求与响应

实测使用 Substrate access token 的 Bearer 鉴权和 `x-anchormailbox`。
本项目的 `REST_API_TOKEN` 只负责外层 API 鉴权，不能代替微软 access token。

响应为 `{ "Value": [...] }`，任务包含 `Id`、`ParentFolderId`、`Subject`、
`Status`、`CommittedDay`、`PostponedDay`、`CommittedOrder`、`OrderDateTime`、
`CompletedDateTime`、`DueDateTime` 等字段，另有 `UncommittedDue`。

## 接入要求与验证边界

- 按项目 `TIMEZONE` 计算本地日期，并始终显式传入 `date`。
- 以 feed 返回的成员为依据，默认保留已完成任务；调用方可以显式排除已完成项。
- 沿用现有单例令牌管理、主机校验、重试、排序和 REST 返回格式。
- 本次小集合没有下一页，不代表永远不分页；需要预算和完整性保护。
- 网页捕获与项目端到端授权分别验证，接入后的结果见下节。
- 网页还包含可选的“自动纳入当天到期任务”逻辑；本次没有这类样本。
- 原先某个 `CommittedDay` 为空的历史已完成任务不在当前浏览器缓存中，未复现。
- 指定过去日期的查询不代表服务保存了每天的历史快照。
- 这是微软内部接口；本文件记录一个账号的实测事实，不代表公开稳定性承诺或
  已完成生产部署。

没有修改微软任务数据。仓库只记录脱敏结论，不保存浏览器令牌、Cookie、
账号地址、任务标题、真实任务 ID 或完整 HAR。

## 接入后的项目验证

2026-10-24，`GET /api/v1/myday` 已切换为读取直接 feed，并继续复用项目的
`TodoIndex` 令牌管理和原有 REST 返回格式。

- 35 个测试文件、387 个测试通过；类型检查及 Workers dry-run 打包通过。
- 使用项目现有本地配置、授权和持久化状态启动本地 Worker，真实调用
  `/api/v1/myday?date=2026-10-24&includeCompleted=true`：HTTP 200，11 个任务，
  其中 1 个已完成；用户刚完成的任务以 `Completed` 返回，仍属于当天 My Day。
- 不传日期的默认请求仍返回本地 24 日的同一组 11 个任务；显式
  `includeCompleted=false` 返回 10 个未完成任务，正确排除刚完成的任务。
- 此次真实读取经过本项目的 REST Bearer 鉴权、Durable Object RPC、Graph 清单
  元数据读取及 Substrate feed，补齐了“只有网页登录 token 验证”的边界。
- 测试覆盖显式本地日期、feed 成员权威性、已完成状态、缺失清单元数据、
  分页去重、截断、坏数据和错误主机；不完整结果不会写回任务的 My Day 字段。
- 本次未部署到 Cloudflare，生产域名仍需部署后单独验收。

## 可靠性修复验证

同日补充审查后完成以下修复，并通过 37 个测试文件、402 个测试及类型检查：

- My Day 读取不再更新或删除共享清单索引；并发旧清单快照不会删除较新同步写入
  的清单和任务。清单删除仍由后台增量同步负责。
- feed 优先读取，Graph 清单元数据最多占用 2 秒；失败或超时使用缓存名称或
  `null`，不影响健康 feed 的任务返回。
- 普通资源 403 冷却 30 秒后重试；明确的权限同意错误保持重新授权要求。
- 整体读取限制为 15 秒，包含等待令牌、网络、正文、分页和退避；超时返回
  `504 my_day_timeout`。共享令牌刷新独立完成，以保存旋转后的 refresh token。
- 重试前释放旧响应正文；MCP 工具对超时返回 `upstream_timeout`。

使用本项目本地配置和授权再次真实读取：默认请求 HTTP 200，11 个任务，其中
1 个已完成，所有任务的清单名称均正常返回；`includeCompleted=false` 为 HTTP
200、10 个未完成任务。没有修改微软任务数据，也没有部署到云端。
