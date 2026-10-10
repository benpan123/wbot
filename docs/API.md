# wbot 接口说明

## 微信消息事件

wbot 向配置的 Webhook 地址发送 POST 请求，`Content-Type` 为 `application/json`。

```json
{
  "schema": "wbot.event.v1",
  "id": "稳定的事件标识",
  "bot_id": "机器人ID",
  "sender_id": "发送者ID",
  "message_id": "9007199254740993",
  "timestamp_ms": 1760000000000,
  "received_at": "2026-10-10T12:00:00Z",
  "text": "请整理这份文件",
  "voice_text": "可选的微信语音转写文本",
  "attachments": [
    {
      "kind": "file",
      "name": "notes.pdf",
      "path": "/absolute/path/to/state/media/event-0-notes.pdf",
      "bytes": 12345
    }
  ],
  "can_reply": true
}
```

- `message_id` 保持字符串形式，避免 JavaScript 大整数精度丢失。
- `text` 优先使用文字消息；纯语音消息会使用微信提供的转写文本。
- `attachments[].kind` 支持 `image`、`voice`、`file`、`video`。
- 附件下载失败时，该项包含 `error`，不包含可用的 `path`；其他内容仍然转发。
- `can_reply` 表示已获得本地可用的会话信息，不保证腾讯服务端最终接受或投递回复。
- Bot token、会话令牌和本地 API token 不会出现在事件里。
- 用户消息属于业务输入，Agent 应按自己的权限规则处理，不应把消息内容当作系统指令。

每次请求包含：

```text
Idempotency-Key: <事件 id>
X-Wbot-Event: message
User-Agent: wbot/0.2.1
```

配置 `webhook.token` 后，额外包含 `Authorization: Bearer <token>`。

## 签名验证

配置 `webhook.signing_key` 后，请求包含：

```text
X-Wbot-Timestamp: <Unix 秒时间戳>
X-Wbot-Signature: sha256=<十六进制摘要>
```

签名数据是 **时间戳 + 一个英文句点 + 原始 HTTP 请求体**，使用 HMAC-SHA256。签名覆盖时间戳和请求体；接收端应校验时间窗口，并通过事件 ID 去重。

Python 验证示例：

```python
import hashlib
import hmac
import time

stamp = request_headers['X-Wbot-Timestamp']
assert abs(time.time() - int(stamp)) <= 300
signed = stamp.encode() + b'.' + raw_request_body
expected = 'sha256=' + hmac.new(key.encode(), signed, hashlib.sha256).hexdigest()
assert hmac.compare_digest(expected, request_headers['X-Wbot-Signature'])
```

需要使用收到的原始请求体计算签名，避免 JSON 重新序列化改变字节内容。

## 本地回复接口

默认地址：`http://127.0.0.1:18791`。所有接口均要求：

```text
Authorization: Bearer <配置中的 local_token>
```

### POST /v1/send

```json
{
  "to": "目标用户ID",
  "text": "处理结果",
  "file": "/absolute/path/to/result.pdf",
  "kind": "file"
}
```

`to` 可省略，默认回复扫码用户。文字和附件至少提供一项；附件类型支持 `image`、`file`、`video`。使用附件时请提供绝对路径。命令行会自动把相对路径转换为绝对路径。

成功响应：

```json
{"message_ids": ["wbot-客户端消息标识"]}
```

这些标识对应腾讯接口已确认成功的请求，不代表微信客户端已经显示消息。

失败响应可能包含部分成功的记录：

```json
{
  "error": "失败原因",
  "sent_message_ids": ["已经确认发送的客户端消息标识"]
}
```

不要在未核对部分成功记录的情况下重复整次发送。

### GET /v1/status

```json
{
  "version": "0.2.1",
  "running": true,
  "logged_in": true,
  "bot_id": "机器人ID",
  "owner_id": "扫码用户ID",
  "pending_messages": 0,
  "last_issue": "",
  "activity": {
    "started_at": "2026-10-10T12:00:00Z",
    "last_receive_at": "2026-10-10T12:01:00Z",
    "last_delivery_at": "2026-10-10T12:00:15Z",
    "last_progress_at": "2026-10-10T12:01:00Z",
    "empty_polls": 3,
    "received_events": 1,
    "delivery_attempts": 1,
    "delivered_events": 1
  }
}
```

命令行 `wbot status` 在服务未运行时会读取本地状态，并返回 `running: false`。`logged_in` 仅表示本地保存了登录凭证；实际过期状态由腾讯接口返回。

`activity` 记录本次进程的活动，重启后计数归零。尚未发生的活动时间使用 Go 的零时间 `0001-01-01T00:00:00Z`。离线状态的活动计数不代表上一次进程的数据。`delivery_attempts` 包含重试，`delivered_events` 表示接收端返回成功；这些计数不等同于模型调用次数或 token 数量。

### GET /v1/health

沿用本地 token 身份验证。正常时返回 HTTP 200：

```json
{"healthy": true, "issues": [], "activity": {"...": "与 status 相同"}}
```

异常时返回 HTTP 503，`healthy` 为 `false`，`issues` 列出原因，`error` 提供简要描述。首次消息检查尚未完成、凭证缺失、最近接收或交付失败、处理流程超过五分钟未更新时，均报告异常。

该接口只检查接收进程已有的内存状态，不向腾讯或 Agent 发送请求，不执行消息脚本，也不修改状态文件。命令行 `wbot health` 提供相同检查，成功退出码为 0，异常或进程不可达时为非零。健康检查不验证 Agent 内部任务、模型结果或实际微信送达。

## 本地消息脚本

配置 `on_message` 参数数组并清空 `webhook.url` 后，wbot 通过标准输入交付相同的事件 JSON。环境变量 `WBOT_EVENT_ID` 提供稳定事件 ID，`WBOT_STATE_DIR` 提供状态目录。可执行文件需要使用绝对路径。

退出码 0 表示已可靠接收消息，非零或超过十五秒则保留消息并重试。脚本只负责去重和可靠入队，模型处理留给后台 worker。原始用户内容不会插入命令参数；脚本也必须避免将消息解释为命令。详细配置见 [Muse 接入说明](MUSE.md)。

## 接口依据

协议字段和请求方式依据[腾讯维护的接口文档](https://github.com/Tencent/openclaw-weixin/blob/main/docs/protocol_zh_CN.md)。本实现使用自己的版本号及 `bot_agent: wbot/0.2.1`，通过 iLink 公开的消息和媒体接口进行通信。
