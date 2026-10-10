# Muse 云端虚拟机接入

目标是让普通程序持续接收微信消息，有新消息时才唤醒 Muse 的 Agent。空闲检查、连接重试和进程管理都由程序处理，无需模型参与。

## 哪些操作会消耗模型 token

| 操作 | wbot 的行为 | 是否调用模型 |
| --- | --- | --- |
| 等待腾讯微信消息 | Go 程序执行 iLink 长轮询 | 否 |
| 空回复、断线重连、读取本地状态 | 普通程序处理 | 否 |
| `wbot health` 健康检查 | 读取接收进程的内存状态 | 否 |
| 新消息交付 | 调用配置的 Webhook 或消息脚本 | 取决于接收端是否启动 Agent |
| systemd 恢复异常退出的进程 | 操作系统重新启动 wbot | 否 |
| Muse 定时唤醒 AI 检查收件箱 | 属于 Muse 的任务配置 | 会产生模型调用 |

wbot 本身没有模型客户端。空闲时不会请求 Agent Webhook，也不会执行消息脚本。已进入待交付队列的消息可以触发重试，因此 Agent 必须按事件 ID 去重，防止重复生成回复。

把 AI 定时检查从每分钟改成每十分钟，仍然会产生空闲调用。需要取消这类 AI 检查任务，改用普通程序的健康检查。模型用量还取决于 Muse 自身的其他定时任务和处理逻辑，wbot 的计数不能替代 Muse 的用量记录。

## 选择消息入口

如果 Muse 已有可接收事件的 HTTP 服务，继续使用 `webhook.url` 即可。wbot 收到消息后立即交付，无需再添加每十五秒检查文件的脚本，也没有固定的一分钟等待。实际回复耗时取决于腾讯消息传输、网络和 Agent 的处理速度。

如果 Muse 已有本地唤醒脚本，可以配置 `on_message`。以下路径仅为占位示例，需要替换为云端虚拟机上真实存在的程序路径：

```json
{
  "listen": "127.0.0.1:18791",
  "local_token": "保留 wbot init 自动生成的值",
  "state_dir": "/absolute/path/to/wbot/state",
  "allow_users": [],
  "webhook": {"url": ""},
  "on_message": ["/absolute/path/to/muse-message-hook"]
}
```

先执行 `wbot init` 创建配置，再修改交付方式。`webhook.url` 与 `on_message` 只能启用一种。切换到脚本方式时，可以移除原来不再使用的 Webhook 密钥配置。

消息脚本的约定：

1. 第一个参数必须是可执行程序的绝对路径，后续元素按原样作为参数传递。也可以使用 `["/usr/bin/python3", "/absolute/path/to/hook.py"]`。
2. 从标准输入读取一条完整 JSON，字段与 [Webhook 事件](API.md#微信消息事件) 相同。`WBOT_EVENT_ID` 环境变量包含事件 ID，`WBOT_STATE_DIR` 包含状态目录。
3. 使用事件 ID 去重，将任务可靠保存到 Muse 的工作队列，再返回退出码 `0`。模型处理和微信回复留给后台 worker。
4. 返回非零退出码、超时或执行失败时，wbot 保留消息并重试；重试沿用同一个事件 ID。单次执行最长十五秒，重试间隔逐步增加到三十秒。
5. 不要把用户消息拼接成 shell 命令。wbot 通过标准输入传递消息；脚本也需要按数据处理。

脚本应快速完成入队，避免在脚本中等待整轮模型回复，或依赖临时后台子进程保存任务。wbot 超时或退出时会终止仍在执行的脚本及其进程组。脚本的标准输出和标准错误不会转发到 wbot 日志；需要诊断时，由脚本自行记录经过脱敏的错误信息。

仓库没有假定 Muse 存在某个固定 CLI 或唤醒 API。需要根据当前 Muse 环境，把上述 JSON 对接到已有的 worker 入口；原来适配 wx-bot 的脚本也需要核对字段，不能假定两者完全兼容。

## 云端后台运行

以下方式适用于支持 systemd 用户服务的 Linux 虚拟机。先在 Muse 虚拟机中安装 wbot、配置实际的消息入口并完成扫码：

```bash
# 在 wbot 仓库目录执行，需要 Go 1.24 或更新版本
./scripts/install.sh
"$HOME/.local/bin/wbot" init
# 此时修改 ~/.config/wbot/config.json，配置 Webhook 或消息脚本
"$HOME/.local/bin/wbot" login
"$HOME/.local/bin/wbot" run
```

先在前台确认微信消息能够交付，Agent 能够使用 `wbot send` 回复。确认后按 Ctrl+C 停止前台进程，再启用后台服务：

```bash
mkdir -p "$HOME/.config/systemd/user"
cp deploy/wbot.service "$HOME/.config/systemd/user/wbot.service"
systemctl --user daemon-reload
systemctl --user enable --now wbot.service
systemctl --user status wbot.service
journalctl --user -u wbot.service -n 50 --no-pager
```

[服务模板](../deploy/wbot.service) 使用默认安装位置和默认配置路径。使用自定义路径时，需要同步修改 `ExecStart`。脚本及其依赖建议使用绝对路径，后台服务的环境变量可能与交互式终端不同。

配置引用了 `env:` 密钥，或消息脚本需要其他环境变量时，请在 `~/.config/wbot/service.env` 中配置，每行使用 `变量名=值`，并将权限设为 `0600`。示例：

```text
WBOT_WEBHOOK_TOKEN=your-secret
WBOT_WEBHOOK_SIGNING_KEY=your-signing-secret
```

该文件使用 systemd 的环境文件格式，不要添加 `export`。手动执行命令时，当前终端也需要具备配置引用的环境变量。

模板会在异常退出后等待十秒再启动进程，五分钟内最多允许五次启动，避免登录过期或配置错误造成无限重启。修复配置或重新扫码后，可以执行：

```bash
systemctl --user stop wbot.service
# 如需重新扫码，在服务停止期间执行 wbot login
systemctl --user reset-failed wbot.service
systemctl --user start wbot.service
```

需要在用户退出登录后继续运行、并随虚拟机开机启动时，管理员可以为当前用户启用 linger：`sudo loginctl enable-linger "$USER"`。如果 Muse 的虚拟机没有 systemd 用户服务，请使用该环境提供的常驻进程管理器执行同一条 `wbot run` 命令。虚拟机被平台暂停、销毁或回收时，进程管理器也无法继续接收消息。

systemd 负责恢复异常退出；进程卡住时的处置需要外部普通监控程序根据健康检查决定。本模板不包含定时 AI 任务，也没有将健康检查失败直接绑定到重启，避免暂时断网引起重启循环。[systemd 服务说明](https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html)

## 验证空闲时没有触发 Agent

```bash
"$HOME/.local/bin/wbot" status
"$HOME/.local/bin/wbot" health
```

`status` 的 `activity` 字段包含本次进程启动以来的计数：

- `empty_polls`：腾讯返回空消息的次数。空闲时增加属于正常现象。
- `received_events`：通过授权检查和去重、进入队列的消息数。
- `delivery_attempts`：向 Webhook 或消息脚本交付的尝试次数，包括失败重试。
- `delivered_events`：接收端已返回成功的交付次数，不代表模型已经回复微信。

在队列已经清空、微信没有新消息的情况下，多次检查时 `delivery_attempts` 应保持不变。发送一条微信消息后，该数值应增加；交付失败时可能增加多次。计数在进程重启后归零，已保存的待交付消息仍会继续重试。

`health` 成功时退出码为 `0`，异常时为非零。刚启动、尚未完成首次微信检查时可能暂时报告异常；连续五分钟没有处理进展、最近收取或交付失败时也会报告异常。该命令只访问本机接收进程，不会触发消息脚本或模型，不会证明 Muse worker 的内部状态正常。

确认新链路真实收发成功后，停用 Muse 中原有的每分钟或每十分钟 AI 收件箱检查；同时停用仍会唤醒同一个 worker 的旧文件轮询脚本，避免重复处理。普通程序监控可以保留。更新仓库不会自动修改 Muse 云端现有任务。
