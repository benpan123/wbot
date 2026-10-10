# wbot

通过微信联系你的智能助手。

微信消息经腾讯 ClawBot / iLink 到达本机 `wbot`，整理成 JSON 后，通过 HTTP POST 转发给你的 Agent。Agent 完成处理后，调用 `wbot send`，就能把结果回复到微信。

```text
微信 / ClawBot  →  wbot  →  Agent Webhook
微信 / ClawBot  ←  wbot  ←  wbot send / 本地 API
```

## 功能

- 微信扫码登录，支持验证码、二维码刷新和登录地址切换。
- 接收文字、图片、文件、视频和语音；语音消息包含微信提供的转写文本。
- 回复文字、图片、文件和视频，单个附件上限 100 MB。
- 接收进度和待转发消息持久化；Agent 暂时不可用时保留消息并自动重试。
- Webhook 支持 Bearer token、HMAC 签名和稳定的消息去重标识。
- 支持收到消息后执行本地脚本，方便连接云端虚拟机上的 Agent worker。
- 空闲轮询和健康检查由普通程序完成，不会定时唤醒 Agent。
- 默认只处理扫码用户的私聊消息，也可以配置允许使用的其他用户。
- 本地回复接口仅监听回环 IP，并要求身份验证。
- 支持 macOS、Linux；每个配置文件对应一个微信账号。

本项目是第三方工具。协议依据来自腾讯维护的[微信接口文档](https://github.com/Tencent/openclaw-weixin/blob/main/docs/protocol_zh_CN.md)。实际可用性还取决于微信账号、ClawBot 接入状态和腾讯接口。

## 快速开始

需要 Go 1.24 或更新版本。

```bash
git clone https://github.com/benpan123/wbot.git
cd wbot
go build -trimpath -o wbot ./cmd/wbot

./wbot init --webhook https://your-agent.example/events
./wbot login
./wbot run
```

使用微信扫描终端二维码，并在手机上确认。`run` 启动后，从微信给 ClawBot 发送第一条消息，程序才会获得回复所需的会话信息。

获取二维码时优先使用腾讯当前文档中的 POST；仅在收到 HTTP 405 时尝试一次 GET 兼容请求。其他错误会直接报告请求方式、接口位置和状态码。有关云端环境的登录排查，见 [登录问题说明](docs/LOGIN.md)。

在另一个终端回复：

```bash
./wbot send --text '你好，我已经收到消息。'
./wbot send --to '<用户ID>' --text '文件已经整理完成。' --file ./summary.pdf
./wbot send --image ./image.png
./wbot send --video ./video.mp4
./wbot status
./wbot health
```

省略 `--to` 时，回复扫码登录的用户。文字会按 UTF-8 字节边界分段，保留原始内容；Markdown 不会自动转成微信富文本。

需要安装到用户命令目录时，可以执行 `./scripts/install.sh`。脚本默认安装到 `~/.local/bin/wbot`，不会自动登录或启动后台服务。

## 连接自己的 Agent

把 `webhook.url` 设置为 Agent 能接收 HTTP POST 的地址。Agent 收到 JSON 后处理 `text` 和 `attachments`，随后执行：

```bash
wbot send --to '<sender_id>' --text '处理结果'
```

Agent 与 wbot 在同一台机器时，也可以调用本地接口。运行在云端的 Agent 需要通过自己的受控执行环境调用本机命令；本项目不提供公网远程控制入口。附件的 `path` 是 wbot 所在机器的绝对路径，远端 Agent 无法直接读取。

仓库提供了一个本地回声示例，适合先检查接入流程：

```bash
./wbot init --webhook http://127.0.0.1:8787/events
python3 examples/echo_agent.py --wbot ./wbot
# 另一个终端依次执行 ./wbot login、./wbot run
```

如果已经执行过 `init`，请直接修改已有配置里的 Webhook 地址。重复执行 `init` 会保留已有配置并提示退出。

### Muse 云端虚拟机

支持通过 Webhook 或 `on_message` 脚本交付消息。只有队列中存在消息时才触发交付；空闲长轮询、健康检查和连接重试均无需模型参与。

[Muse 接入与后台运行说明](docs/MUSE.md)包含脚本接口、systemd 服务和空闲验证方法。无需额外创建每分钟唤醒 AI 的任务；已有的 Muse 定时检查需要在确认新链路可用后停用。

## 配置

默认配置：`~/.config/wbot/config.json`。

```json
{
  "listen": "127.0.0.1:18791",
  "local_token": "由 wbot init 自动生成",
  "state_dir": "/absolute/path/to/wbot/state",
  "allow_users": [],
  "webhook": {
    "url": "https://your-agent.example/events",
    "token": "env:WBOT_WEBHOOK_TOKEN",
    "signing_key": "env:WBOT_WEBHOOK_SIGNING_KEY"
  }
}
```

`token` 和 `signing_key` 均为可选项。配置了 `env:` 引用时，需要在运行环境中设置对应变量；程序发现变量缺失会提示退出。

`allow_users` 为空时只接受扫码用户；非空时仅接受列表中的用户。更改配置后需要重新启动程序。新创建的配置和登录状态文件使用 `0600` 权限，专用目录使用 `0700` 权限。

多个账号可以使用不同配置和端口：

```bash
wbot --config ./profiles/work.json init --webhook https://agent.example/work
wbot --config ./profiles/work.json login
wbot --config ./profiles/work.json run
```

每份配置的 `state_dir` 和 `listen` 必须不同。同一配置只能运行一个接收进程。运行期间需要停止进程后再登录；待转发队列非空时，先完成转发再重新登录，避免覆盖尚未送达的消息。

## Webhook 数据与回复 API

完整字段、签名验证方式、回复接口和错误处理见 [接口说明](docs/API.md)。

程序采用“至少送达一次”的转发方式：崩溃发生在 Agent 接收成功、wbot 保存确认之前时，消息可能再次投递。Agent 应通过 `Idempotency-Key` 或 JSON 的 `id` 去重。持久化去重窗口保留最近 2048 个已完成事件。

Agent 返回非 2xx 状态时，当前消息会保留，后续消息按顺序等待；重试间隔逐步增加到 30 秒。请在 Agent 可靠保存任务后返回 2xx，耗时任务留在后台处理，避免占用 15 秒的 Webhook 请求窗口。

回复发送结果不明确时，程序会报告错误，避免自动重复发送。文字加附件可能涉及多个微信请求；部分成功时，错误信息会包含已经确认发送的数量。

## 开发与验证

```bash
go test -race -timeout 60s ./...
go vet ./...
go build -trimpath -o wbot ./cmd/wbot
python3 tests/smoke.py ./wbot
make cross
```

测试使用本地模拟的微信接口和 Agent，覆盖登录、授权、持久化重试、去重、签名、附件加密和完整收发流程。模拟测试通过不代表真实微信账号已经接入；真实收发仍需要扫码和 Agent 地址。

空闲回归测试同时覆盖 Webhook 和消息脚本，验证连续空轮询及健康检查不会触发 Agent、状态未改变时不会重新写入文件，并检查脚本失败重试及取消行为。

当前支持私聊，不处理群消息；语音转写依赖微信提供的文本，语音文件保持原编码，暂不发送语音消息。附件保存在 `state_dir/media`，需要自行管理保留时间和磁盘空间。

## 许可证

[MIT](LICENSE)。二维码显示等依赖的许可声明见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
