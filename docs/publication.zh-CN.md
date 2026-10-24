# GitHub 与 GHCR 发布验证

本仓库从已验证的 Go 重写实现中独立导出，Go 模块位于根目录，不包含旧版源码、旧 Git 历史、运行数据库、`.env` 或真实授权数据。

- 参考原项目：[dszp/mstodo-mcp-cloudflare](https://github.com/dszp/mstodo-mcp-cloudflare)。
- 当前仓库：[qaq112233/mstodo-go](https://github.com/qaq112233/mstodo-go)。
- 构建流程：[Build and publish](https://github.com/qaq112233/mstodo-go/actions/workflows/build.yml)。
- 镜像：`ghcr.io/qaq112233/mstodo-go`，计划同时提供 `linux/amd64` 与 `linux/arm64`。

## 首次发布

当前正准备首次推送与 Actions 验证。创建仓库、触发 workflow 或文件中声明双架构，均不能替代实际完成的构建和镜像索引检查。首次运行完成后在此补充运行地址、镜像摘要和验收结果。

## 发布规则

workflow 先运行测试、竞争检测、vet 和 Compose 配置检查，再构建两个架构的静态程序；只有成功后才发布镜像。镜像发布使用权限限于当前仓库的 `GITHUB_TOKEN`，无须在仓库配置个人 GHCR 密钥。

PR 仅执行测试与程序构建。main、`v*` 标签和手动运行可发布；镜像标签包括 main 的 latest、提交 SHA，以及版本标签对应的 semver 标签。Dockerfile 在构建平台交叉编译两个目标，scratch 运行层不执行目标架构命令，无须 QEMU。

发布后 workflow 会检查真实镜像索引至少包含 linux/amd64 和 linux/arm64，并运行已发布 amd64 镜像的健康检查；arm64 的实际设备运行仍需单独验收。

GHCR 新包的可见性可能需要在包设置中设为公开，和源码仓库可见性分别管理。镜像公开后可匿名拉取；私有镜像需具有 read:packages 的 GitHub PAT（classic）登录。参考：[GitHub Container registry](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)。
