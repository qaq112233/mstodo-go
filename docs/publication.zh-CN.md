# GitHub 与 GHCR 发布验证

本仓库从已验证的 Go 重写实现中独立导出，Go 模块位于根目录，不包含旧版源码、旧 Git 历史、运行数据库、`.env` 或真实授权数据。

- 参考原项目：[dszp/mstodo-mcp-cloudflare](https://github.com/dszp/mstodo-mcp-cloudflare)。
- 当前仓库：[qaq112233/mstodo-go](https://github.com/qaq112233/mstodo-go)。
- 构建流程：[Build and publish](https://github.com/qaq112233/mstodo-go/actions/workflows/build.yml)。
- 镜像：`ghcr.io/qaq112233/mstodo-go`，已经提供 `linux/amd64` 与 `linux/arm64`。

## 首次发布

2026-10-24 已完成首次真实发布：

| 项目 | 结果 |
| --- | --- |
| 源码提交 | `75e927166b8fa7d8c6b1eb825588ac74ce4a757f` |
| Actions | [运行 40091537627](https://github.com/qaq112233/mstodo-go/actions/runs/40091537627)，全部 job 成功 |
| 自动化 | 42 个顶层 Go 测试、竞争检测、vet、Compose 配置检查通过 |
| Go 构建 | Linux amd64 和 arm64 静态程序、各自 SHA-256 与 MIT 许可已上传为 Actions artifacts，保存 14 天 |
| Docker 构建 | 双架构构建及 GHCR 推送成功 |
| 架构 | 实际发布索引包含 `linux/amd64` 与 `linux/arm64` |
| 容器运行 | Actions 拉取已发布 amd64 镜像，在非 root、只读文件系统下通过健康检查 |
| 公共访问 | 使用匿名 GHCR token 读取 latest 索引成功，无须登录 |

首次镜像的固定引用：

```text
ghcr.io/qaq112233/mstodo-go@sha256:0c261c3298c13ec52421fa2d3af7db73dbb89280861bd2f574fb8549ac3fb988
```

对应标签为 `latest` 和 `sha-75e927166b8fa7d8c6b1eb825588ac74ce4a757f`。后续 main 推送可能更新 latest；以上 digest 和提交标签用于识别本次发布。发布记录自身的文档提交不改变已经验证的应用源码。

索引还包含两个带 attestation-manifest 标记的构建证明条目，显示为 unknown/unknown；它们不是额外运行架构。arm64 镜像已构建发布，但没有在真实 ARM 设备上启动。生产 HTTPS、墨水屏和最终 MCP 产品客户端仍未验收。

原有 TypeScript 仓库和部署未改动，本独立仓库是新的 Git 历史，未推送真实凭据或测试运行数据。

## 发布规则

workflow 先运行测试、竞争检测、vet 和 Compose 配置检查，再构建两个架构的静态程序；只有成功后才发布镜像。镜像发布使用权限限于当前仓库的 `GITHUB_TOKEN`，无须在仓库配置个人 GHCR 密钥。

PR 仅执行测试与程序构建。main、`v*` 标签和手动运行可发布；镜像标签包括 main 的 latest、提交 SHA，以及版本标签对应的 semver 标签。Dockerfile 在构建平台交叉编译两个目标，scratch 运行层不执行目标架构命令，无须 QEMU。

发布后 workflow 会检查真实镜像索引至少包含 linux/amd64 和 linux/arm64，并运行已发布 amd64 镜像的健康检查；arm64 的实际设备运行仍需单独验收。

当前镜像已确认为公开并可匿名读取。将 workflow 用于其他仓库时，注意 GHCR 包的可见性和源码仓库分别管理；私有镜像需具有 read:packages 的 GitHub PAT（classic）登录。参考：[GitHub Container registry](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)。
