# Go 首版 MCP 接入

使用官方 [`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk) 的 Streamable HTTP，REST 与 MCP 直接调用同一 Go 业务层，共享 SQLite、180 秒缓存、令牌刷新和请求限速，不相互发送 HTTP 请求。

## 地址和访问模式

| 模式 | MCP URL | 请求头 |
| --- | --- | --- |
| bearer（默认） | `https://todo.example.com/mcp` | `Authorization: Bearer <业务token>` |
| path | `https://todo.example.com/s/<secret>/mcp` | 无业务 Bearer |
| none | `https://todo.example.com/mcp` | 无业务验证；仍遵守外层规则 |

先在设置页完成微软授权，再连接 MCP；客户端不需要重新登录微软。业务 token 不能替代微软 token，管理员凭据也不应用作业务 token。

支持能手动配置 URL/header 的客户端；首版不提供面向 MCP 客户端的 OAuth，不承诺兼容只接受 OAuth 的客户端。最终客户端尚未选定，不把通用示例标成真实客户端实测配置。

通用配置示意（客户端实际字段可能不同）：

```json
{
  "mcpServers": {
    "mstodo": {
      "url": "https://todo.example.com/mcp",
      "headers": {"Authorization": "Bearer <业务token>"}
    }
  }
}
```

path 模式将 URL 换成秘密前缀地址，移除业务 Authorization。none 只在明确开放或外层已验证时使用。若 Cloudflare Access 要求 Service Token，客户端需同时携带它的请求头。反向代理保留 MCP 相关头和 POST body；服务采用无会话模式和 JSON 响应，GET/DELETE MCP 请求由 SDK 返回 405。

## 工具与参数

共 32 个工具。所有读取工具可传 `refresh`；My Day 可传 `date` 和 `include_completed`，默认保留已完成项。

| 工具 | 主要参数 |
| --- | --- |
| list_my_day_tasks | date、include_completed、refresh |
| list_lists / create_list | refresh / displayName |
| get_list / update_list / delete_list | list；更新另传 displayName |
| list_tasks / list_tasks_by_manual_order | list、limit、offset、status、refresh；前者可传 order=manual |
| create_task | list、title 和任务字段 |
| get_task / update_task / delete_task | list、task_id；更新另传任务字段和可选 if_match |
| query_tasks / search_tasks | q、list_ids、status、importance、dueFrom、dueTo、modifiedSince、limit、offset、refresh |
| add_to_my_day / remove_from_my_day | list、task_id；加入可传 date |
| move_task | list、task_id、to_list |
| list_checklist_items / create_checklist_item | list、task_id；创建传 displayName、可选 isChecked |
| get_checklist_item / update_checklist_item / delete_checklist_item | list、task_id、item_id；更新传 displayName/isChecked |
| list_linked_resources / create_linked_resource | list、task_id；创建传 applicationName、displayName、webUrl、externalId |
| get_linked_resource / update_linked_resource / delete_linked_resource | list、task_id、resource_id；更新传链接字段 |
| reorder_task | list、task_id、position，视位置传 index/reference_task_id |
| reorder_checklist_item | list、task_id、item_id、position，视位置传 index/reference_item_id |
| reorder_my_day_task | list、task_id、position、可选 date/index/reference_task_id/reference_list_id |
| whoami / sync_status | refresh（状态只读本地，不强制探测微软） |

`list` 和 `to_list` **均为微软清单 ID**。首版无旧分类、别名和名称解析。ID 可以先通过 list_lists 获取。search_tasks 的 q 必填，query_tasks 可不传 q。分页默认 50、最大 200，offset 从 0 开始。

任务字段支持 title、body、status、importance、isReminderOn、categories、recurrence，以及 Graph 的 dueDateTime/startDateTime/reminderDateTime/completedDateTime。为保留常用参数，也支持：

- `body` 传字符串时转成 Graph 文本正文；也可传 `{contentType,content}`。
- `due`、`start`、`reminder`、`completed_date` 转成 UTC dateTimeTimeZone 对象。
- 日期和 recurrence 传 null 表示清空；省略不修改。
- 正常完成传 `status:"completed"`，恢复传 `status:"notStarted"`。历史补记 completed_date 仅转译字段，不自动执行多次状态切换。

同一字段请不要同时使用 Graph 名称和简写。提醒和重复由微软执行，Graph 字段及状态大小写见 [REST 文档](go-rest-api.zh-CN.md)。查询只搜索标题和正文，中文关键词按包含处理，不承诺旧 FTS 语法、步骤全文搜索或旧排序。

## 读取、写入和错误

My Day 与 REST 读取同一个直接 feed 快照，不再使用旧 MCP 的字段推测缓存。不同完成筛选共享原始缓存；过期刷新失败会报错，不将旧数组包装为成功。任务写入、移动、清单修改和排序后会失效相关快照，另一入口下次读取重新获取。

工具提供输入 schema 和 readOnly/destructive annotations。服务不在每次写入前自建人工审批；客户端可根据自己的策略确认。删除、修改和移动会标记其修改效果，创建不会被误标为只读。

业务错误使用 SDK 的 `isError:true`，文本内容为与 REST 对应的 JSON：

```json
{"error":"my_day_unavailable","retryAfter":30}
```

结果未确认时返回 write_outcome_unknown 或 move_outcome_unknown，并标记 outcomeUnknown。不要因此再次创建或复制任务，先核对微软网页。移动成功返回 previousTaskId 和 taskId，后续使用新 ID；原附件保留依赖原任务 re-parent，未提供附件上传下载工具。

已确认微软写入而缓存失败时，成功结果附 cacheWarning；删除成功返回 `{"ok":true}`。客户端应展示警告但不要把它当作未执行写入。

## 验证边界

自动化集成测试使用官方 SDK 客户端，通过本地 HTTP 连接分别测试 bearer、path、none，检查 32 个工具、业务错误以及 REST/MCP 共用 My Day 缓存。另已用官方 SDK 通过 Bearer 读取真实账号，11 个 My Day 任务的完整字段和顺序与 REST 一致；本次真实业务写入通过 REST 验证。这不等于最终客户端、生产 HTTPS 或 Cloudflare Access 验收。实测状态见 [验证记录](go-validation.zh-CN.md)。
