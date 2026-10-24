# Go 首版 REST API

这份文档对应当前独立 Go 服务。旧 TypeScript 的 REST 指南保留在参考项目中，本仓库不包含旧版源码。真实部署验收状态见 [Go 验证记录](go-validation.zh-CN.md)。

## 访问模式

所有业务路由共用设置页选定的模式：

| 模式 | 地址 | Header |
| --- | --- | --- |
| bearer（默认） | `https://todo.example.com/api/v1/...` | `Authorization: Bearer <token>` |
| path | `https://todo.example.com/s/<secret>/api/v1/...` | 无需业务 Bearer |
| none | `https://todo.example.com/api/v1/...` | 无业务验证；按外层访问规则调用 |

path 模式下，未带秘密前缀的业务路径返回 404。设置网页使用独立 `/admin/<secret>`，不会因 none 模式开放。默认 token、路径凭据、管理员凭据均由至少 32 字节安全随机值产生，可在网页轮换。不要将含凭据的地址写入公开日志。

后文用 `BASE` 表示对应模式的地址前缀（含 `/s/<secret>`，不含 `/api/v1`）。Bearer 模式示例：

```bash
export BASE=https://todo.example.com
read -rsp '业务 token: ' MSTODO_TOKEN
echo
curl --fail-with-body -H "Authorization: Bearer $MSTODO_TOKEN" \
  "$BASE/api/v1/myday"
```

响应 JSON，`Cache-Control: no-store`。创建 201、读取和修改 200、删除 204。GET 的 `refresh=true|false` 默认为 false；不能绕过并发合并、请求限速和微软 Retry-After。分页参数为整数；调用方应 URL 编码路径中的 ID 和查询参数。

## My Day

```http
GET /api/v1/myday?date=2026-10-24&includeCompleted=true&refresh=false
```

`date` 必须为真实的 `YYYY-MM-DD`，默认使用设置时区的今天。`includeCompleted` 仅允许小写 true 或 false，默认 true。指定过去日期只是读取相应 feed，不是历史快照。

```json
{
  "date": "2026-10-24",
  "timezone": "Asia/Shanghai",
  "count": 1,
  "tasks": [{
    "id": "task-id", "listId": "list-id", "listName": "工作",
    "title": "周报", "status": "NotStarted", "importance": "Normal",
    "dueDate": "2026-10-24", "startDate": null, "completedDate": null,
    "isReminderOn": false, "reminderDate": null, "categories": [],
    "committedDay": null, "committedOrder": null, "orderDateTime": null,
    "postponedDay": null, "lastModifiedDate": null
  }]
}
```

保留旧成功字段和 Substrate 状态大小写。feed 决定成员，不按 committedDay、截止日期或 postponedDay 再过滤。名称缺失为 null，不删除任务；categories 总为数组。顺序为 committedOrder 降序、orderDateTime 降序、清单 ID 和任务 ID 升序。不同完成筛选共用原始完整快照。

完整成功结束后缓存 180 秒，跨天和不同时区使用独立键；有效空列表也缓存。所有页读取失败、无效身份或结构、分页不完整均返回错误，不返回部分任务。15 秒期限涵盖令牌、限速、网络、正文、分页和退避；清单名称最多补充 2 秒。过期缓存不会在刷新失败后作为成功返回。

```text
X-Cache: HIT 或 MISS
X-Data-Fetched-At: 成功读取完成时的 UTC 时间
X-Cache-TTL: 180
```

这些是内部缓存诊断；HTTP 本身仍为 no-store。墨水屏遇到错误时保留上次成功画面。

## 路由总表

以 `L=/api/v1/lists/{listId}`、`T=L/tasks/{taskId}` 简写；下面的 L/T 均需展开成实际路径。

| 路由 | 方法 | 功能 |
| --- | --- | --- |
| `/api/v1/lists` | GET / POST | 清单目录 / 创建 |
| `L` | GET / PATCH / DELETE | 清单详情 / 改名 / 删除 |
| `L/tasks` | GET / POST | 清单任务查询 / 创建 |
| `T` | GET / PATCH / DELETE | 任务详情 / 修改 / 删除 |
| `/api/v1/tasks` | GET | 跨清单查询和搜索 |
| `T/myday` | POST / DELETE | 加入 / 移出 My Day |
| `T/move` | POST | 移到另一清单 |
| `T/checklist-items` | GET / POST | 步骤集合 / 创建 |
| `T/checklist-items/{itemId}` | GET / PATCH / DELETE | 单个步骤 |
| `T/linked-resources` | GET / POST | 关联链接集合 / 创建 |
| `T/linked-resources/{resourceId}` | GET / PATCH / DELETE | 单个链接 |
| `T/reorder` | POST | 清单内任务排序 |
| `T/checklist-items/{itemId}/reorder` | POST | 步骤排序 |
| `/api/v1/myday/reorder` | POST | My Day 排序 |
| `/api/v1/status` | GET | 账号、权限问题及清单同步状态 |
| `/health`（公开、不加秘密前缀） | GET | 进程存活，不探测微软 |

`GET /lists`、步骤集合和链接集合返回 JSON 数组；任务详情等返回 Graph 资源对象。任务查询返回下节的分页对象。ID 使用微软实际值，首版无别名解析。

## 清单和任务字段

清单 POST/PATCH：`{"displayName":"测试清单"}`。任务 POST 至少提供 title，PATCH 只修改明确给出的字段：

```json
{
  "title": "周报",
  "body": {"contentType": "text", "content": "整理本周进度"},
  "status": "notStarted",
  "importance": "high",
  "dueDateTime": {"dateTime": "2026-10-25T00:00:00", "timeZone": "UTC"},
  "reminderDateTime": {"dateTime": "2026-10-25T01:00:00", "timeZone": "UTC"},
  "isReminderOn": true,
  "categories": ["工作"]
}
```

支持 `title`、`body`（text/html）、`status`、`importance`、`categories`、`isReminderOn`、`dueDateTime`、`startDateTime`、`reminderDateTime`、`completedDateTime`、`recurrence`。提醒和重复由微软执行，本服务无自己的提醒引擎。

普通任务使用 Graph 大小写：status 为 `notStarted/inProgress/completed/waitingOnOthers/deferred`；importance 为 `low/normal/high`。完成任务 PATCH `{"status":"completed"}`，恢复 PATCH `{"status":"notStarted"}`。微软对重复任务完成可能推进到下次 occurrence。

清空日期、提醒和重复规则必须明确传 null，省略表示不修改，例如：

```json
{"dueDateTime":null,"reminderDateTime":null,"isReminderOn":false,"recurrence":null}
```

`recurrence` 使用 Graph `{pattern,range}` 对象，例如每天：

```json
{"recurrence":{"pattern":{"type":"daily","interval":1},"range":{"type":"noEnd","startDate":"2026-10-24"}}}
```

微软字段约束以 [todoTask 更新接口](https://learn.microsoft.com/en-us/graph/api/todotask-update?view=graph-rest-1.0) 为准。直接设置 completedDateTime 的历史补记不保证微软产生实际状态转换；正常完成请使用 status。首版不自动拆成多次重置状态写入。

PATCH/DELETE 可发送 `If-Match: <@odata.etag>`，微软发现冲突返回 412；不要无条件覆盖。

## 查询与搜索

```http
GET /api/v1/tasks?q=周报&listId=L1&listId=L2&status=notStarted&importance=high&dueFrom=2026-10-01&dueTo=2026-10-31&modifiedSince=2026-10-01T00:00:00Z&limit=50&offset=0
```

`q` 搜标题和正文的字面子串，支持中文，`%`、`_` 按普通字符处理，不解释 SQL/FTS 表达式。筛选包括可重复 listId、status、importance、包含边界的 dueFrom/dueTo 和 RFC3339 modifiedSince。省略 listId 覆盖 Graph 可读取的全部清单，包括标记邮件清单；特殊清单写能力由微软决定。

默认 limit 50、最大 200，offset 默认 0。固定按最近修改时间降序、清单 ID 和任务 ID 升序。跨请求分页允许任务正常变动，不创建搜索会话。

```json
{
  "tasks": [{"id":"task-id","listId":"L1","title":"周报","status":"notStarted"}],
  "total": 1, "limit": 50, "offset": 0,
  "dataFetchedAt": "2026-10-24T01:00:00Z"
}
```

dataFetchedAt 为相关清单最早成功更新时间。清单首次建立任务基线，超过 180 秒才更新所需范围的 delta；本轮所有分页结束才标记新鲜。失败保留分页游标，下次请求继续，不返回部分搜索结果。普通查询最多 60 秒，失败可返回 `syncIncomplete:true`。空闲时不同步任务，不为搜索逐任务读取步骤或链接。

`GET L/tasks?order=manual` 读取完整 Substrate 清单顺序并缓存，再用 limit/offset 分页，可按 status 筛选；其字段投影使用 camelCase，增加 orderDateTime。手动排序读取不支持其他搜索条件；搜索应使用普通查询接口。

## 步骤、链接、My Day、移动与排序

步骤创建：`{"displayName":"收集数据","isChecked":false}`；PATCH 修改 displayName/isChecked。步骤集合会读取 inline Subtasks 补充 orderDateTime，按手动顺序降序，未排序项按 createdDateTime 升序；Graph 和 Substrate 任一必要读取失败会报错。

链接创建：`{"applicationName":"资料库","displayName":"参考","webUrl":"https://example.com/note","externalId":"note-1"}`。PATCH 支持同样字段；微软可能限制每任务的链接数量，服务不伪造多链接成功。

加入 My Day：`POST T/myday`，body 可为 `{}` 或 `{"date":"2026-10-24"}`；设置 CommittedDay、清 PostponedDay 并种下 CommittedOrder。移出：`DELETE T/myday`，沿用基线清空 CommittedDay 的请求。成功修改返回 Substrate 回显对象；下一次读取仍重新获取 feed。

移动：`POST T/move`，body `{"toListId":"目标清单ID"}`。仅 PATCH 原任务 ParentFolderId，保留其子资源；不复制后删除。成功：

```json
{"previousTaskId":"旧ID","taskId":"新ID","fromListId":"L1","toListId":"L2"}
```

后续调用必须使用新 ID。返回丢失或目标/ID 无法确认会得到 `move_outcome_unknown`；请先核对微软网页，不要盲目重复。

三个排序入口使用 `{"position":"top"}`、bottom、`{"position":"index","index":2}`（1 起）、或 before/after。任务用 `referenceTaskId`，步骤用 `referenceItemId`。My Day 排序 body 还需 `listId` 和 `taskId`，可传 date 和 `referenceListId`。例：

```json
{"listId":"L1","taskId":"A","position":"before","referenceTaskId":"B","referenceListId":"L2","date":"2026-10-24"}
```

My Day 邻居来自完整当日 feed，排除已完成任务；只写 CommittedOrder。清单和步骤排序只写 OrderDateTime；三者相互独立。无毫秒中点空间时返回 `order_precision_exhausted`。首版无原始时间戳 set 模式。

## 错误、冷却与已确认写入

```json
{"error":"my_day_timeout"}
```

错误可能附 upstreamStatus、retryAfter、outcomeUnknown、syncIncomplete；绝不附原始微软响应、token 或任务正文。错误响应不包含成功任务数组。

| HTTP | 错误示例 | 处理 |
| --- | --- | --- |
| 400 | invalid_date / invalid_include_completed / invalid_refresh / invalid_fields | 修正参数或字段 |
| 401 | unauthorized | 更新业务凭据 |
| 404 / 405 | not_found / method_not_allowed | 检查路径、ID、方法；405 带 Allow |
| 409 | data_changed_retry / order_precision_exhausted | 并发修改后重读或选择 top/bottom |
| 412 | graph_error（upstreamStatus=412） | ETag 冲突，重新读取 |
| 429 | substrate_error / upstream_rate_limited | 遵守 Retry-After；手动刷新也不豁免 |
| 502 | graph_error / substrate_error / my_day_truncated / delta_incomplete | 上游错误或不完整集合，不当成空结果 |
| 502 | write_outcome_unknown / move_outcome_unknown | 结果未确认，先核对微软，不盲目重试 |
| 503 | not_authenticated / microsoft_reauth_required / my_day_unavailable | 设置页授权或检查权限 |
| 504 | my_day_timeout / upstream_timeout | 保留上次结果；聚合查询下次继续已保存分页 |
| 500 | internal_error | 检查服务和本地存储 |

普通资源 403 冷却 30 秒；明确未同意 Exchange 的状态需要重新授权。GET 临时网络/5xx 最多重试一次；明确的 401 最多刷新一次，429 在预算内遵守冷却。写入不因网络或响应丢失自动重发。

微软写入已确认而本地缓存失败时，REST 仍返回 200/201/204，并附 `X-Cache-Warning`；不要因此重复创建。服务将使后续读取重新同步。写入不会将整清单的成功时间更新为现在。
