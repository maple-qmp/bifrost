# Bifrost AI Gateway · maple-qmp fork

这是基于 [maximhq/bifrost](https://github.com/maximhq/bifrost) 维护的独立 fork。Bifrost 提供统一的 AI Gateway、OpenAI 兼容接口、MCP 工具调用、Provider 管理和 Web 控制台；本仓库在此基础上加入本地账号 Provider 与本地安装方式。

> **获取本 fork 的版本：**从本仓库的 `dev` 分支拉取源码并在本地构建。目前没有发布此 fork 的 npm 包、Docker Hub 镜像或预编译二进制文件。`npx -y @maximhq/bifrost`、`docker run maximhq/bifrost` 和 `go get github.com/maximhq/bifrost/...` 获取的是上游版本，**不包含本仓库的改动**。

## 本 fork 的改动

- **ChatGPT Plan 与 CodeBuddy 本地账号 Provider：**直接在 Bifrost 中连接本机账号，不需要单独运行 AI-Proxy、Python 或 LiteLLM 服务。支持 Chat Completions、Responses 和 Anthropic 兼容消息接口；可用模型和能力取决于实际账号。
- **账号请求转换与工具调用修复：**保留本地账号请求在不同 API 格式之间转换时的工具调用与结果历史。
- **macOS 本地 Homebrew 安装：**从当前检出的源码构建网关和管理界面，生成只保存在本机的 formula，并可作为 `brew services` 服务运行。它不会发布到远程 tap。

账号连接、支持范围及限制请看 [本地账号使用文档](docs/providers/supported-providers/local-accounts.mdx)。底层实现与验证说明见 [localaccount README](core/providers/localaccount/README.md)。

## 快速开始：macOS 本地服务

需要 Homebrew、Go 1.27.3、Node.js（仓库 `.nvmrc` 指定 22.12.0）、Python 3，以及 Xcode Command Line Tools。安装脚本会从**本仓库源码**构建二进制和 Web UI，然后安装到本机的 `maple/bifrost-local` tap。

~~~bash
git clone -b dev https://github.com/maple-qmp/bifrost.git
cd bifrost
scripts/homebrew/install-local.sh
brew test bifrost-local
brew services start bifrost-local
~~~

打开 [http://127.0.0.1:8080](http://127.0.0.1:8080)。首次创建管理员时，在终端读取并输入初始化令牌：

~~~bash
bifrost-local setup-token
~~~

服务只监听本机回环地址。账号连接需要在运行 Bifrost 的同一台电脑上操作：在 **Model Providers** 中添加 **ChatGPT Plan** 并完成浏览器登录，或先登录 CodeBuddy 桌面应用再选择 **CodeBuddy → Detect account**。ChatGPT 登录回调使用本机 1455 端口。

安装后的数据保存在 `~/Library/Application Support/BifrostLocal`。更新源码后重新运行 `scripts/homebrew/install-local.sh`，再执行 `brew services restart bifrost-local`；仅运行 `brew upgrade` 不会获取新的仓库代码。服务控制、日志位置和卸载方法见 [Homebrew 安装说明](scripts/homebrew/README.md)。

## 从源码构建和运行

适用于不使用上述 Homebrew 服务的开发环境。准备 Go 1.27.3、Node.js 22.12.0、npm、make 和可用于 CGO/SQLite 的 C 编译工具。请在**仓库根目录**运行，并传入 `LOCAL=1`，让多模块构建使用当前 `go.work` 中的本地源码。

~~~bash
git clone -b dev https://github.com/maple-qmp/bifrost.git
cd bifrost
make build LOCAL=1
~~~

构建会生成包含管理界面的 `tmp/bifrost-http`。以下示例使用独立的本地数据目录，仅监听回环地址，并在首次创建管理员前设置初始化令牌：

~~~bash
mkdir -p tmp/local-data
export BIFROST_SETUP_TOKEN="$(openssl rand -hex 32)"
printf 'Setup token: %s\n' "$BIFROST_SETUP_TOKEN"
./tmp/bifrost-http -host 127.0.0.1 -port 8080 -app-dir ./tmp/local-data
~~~

然后打开 [http://127.0.0.1:8080](http://127.0.0.1:8080)，创建管理员并配置 Provider。服务健康检查可用 `curl http://127.0.0.1:8080/health`。若开启推理接口鉴权，请先在控制台创建凭据或虚拟 Key，再让客户端调用 API。

如果需要容器，可使用 `make docker-image LOCAL=1` **从本仓库源码构建本地镜像**；这与拉取上游的 `maximhq/bifrost` 镜像不同。CodeBuddy 桌面凭据发现和 ChatGPT 本机回调依赖宿主机环境，使用容器时需自行处理这些访问路径；本地账号场景建议优先使用本机运行方式。

## 使用本地账号

在控制台连接账号后，从 **Available models** 选择该账号实际可用的模型。请求使用显式 Provider 前缀，例如 `chatgpt/<model>` 或 `codebuddy/<model>`。模型名称不是固定清单，能否调用仍受账号权限和额度限制。

~~~bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer <your-virtual-key>' \
  -d '{
    "model": "chatgpt/<model>",
    "messages": [{"role": "user", "content": "Hello"}]
  }'
~~~

当推理接口未启用鉴权时，可以省略 `Authorization` 请求头。账号 Provider 不接受常规 API Key；连接与凭据管理都在本机完成。ChatGPT 计划额度不是 OpenAI API 计费额度，CodeBuddy 的可用能力也以桌面账号和上游响应为准。详细限制与配置示例见[本地账号使用文档](docs/providers/supported-providers/local-accounts.mdx)。

## 开发与维护

~~~bash
make dev                               # UI 与 API 热重载
make build LOCAL=1                     # 用当前源码构建网关
go test ./core/providers/localaccount  # 本地账号 Provider 单元测试
make test-mcp                          # 基于 mock 的 MCP/Agent 测试
~~~

`make test-core` 的其他 Provider 测试可能调用真实服务并需要相应凭据。需要同步原项目更新时，将原项目作为 `upstream` 远端，只把更新合入**本 fork**：

~~~bash
git remote add upstream https://github.com/maximhq/bifrost.git  # 首次配置
git fetch upstream
git switch dev
git merge upstream/dev
~~~

解决冲突并验证后，向自己的 `origin/dev` 推送。本仓库不会通过上游 npm 包、上游 Docker 镜像或上游 Go 模块分发本 fork 的功能。

## 文档与来源

- [本地账号使用文档](docs/providers/supported-providers/local-accounts.mdx)
- [macOS Homebrew 本地安装说明](scripts/homebrew/README.md)
- [上游 Bifrost 文档](https://docs.getbifrost.ai/)：适合了解通用网关功能；其中的上游安装命令不适用于获取本 fork 的改动
- [上游项目](https://github.com/maximhq/bifrost)

本项目沿用 [Apache 2.0 许可证](LICENSE)。Bifrost 原项目由 [Maxim](https://github.com/maximhq) 开发和维护；本仓库是独立维护的 fork。
