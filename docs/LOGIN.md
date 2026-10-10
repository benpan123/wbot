# 登录二维码问题排查

## 获取二维码时返回 HTTP 405

405 表示响应方不接受当前请求方式。响应可能来自腾讯接口，也可能来自虚拟机网络中的代理或网关；仅凭状态码无法判断具体来源。生成二维码失败发生在扫码之前，不能据此认定微信账号异常。

腾讯当前的[协议文档](https://github.com/Tencent/openclaw-weixin/blob/main/docs/protocol_zh_CN.md)和[登录实现](https://github.com/Tencent/openclaw-weixin/blob/main/src/auth/login-qr.ts)使用：

```text
POST https://ilinkai.weixin.qq.com/ilink/bot/get_bot_qrcode?bot_type=3
{"local_token_list":[]}
```

wbot 0.2.1 保留这个默认请求。当该二维码接口明确返回 HTTP 405 时，程序自动尝试一次相同地址的 GET 请求。兼容请求不包含旧 token、请求体或 POST 专用请求头。此逻辑仅适用于生成二维码，不适用于消息发送，也不会在 403、429、服务器错误或网络超时后自动切换请求方式。

2026-10-10 的本机实测中，腾讯接口的 POST、GET 均返回 HTTP 200 和完整二维码字段；wbot 0.2.0 本身也成功显示二维码。这证明本机环境能够获取二维码，无法证明所有云端环境都具有相同结果。0.2.1 的 POST→405→GET 行为另有本地模拟回归测试。

## 更新云端已安装的版本

在原来的 wbot 仓库目录执行；如果存在本地代码修改，先保留这些修改，不要强制覆盖：

```bash
git pull --ff-only
./scripts/install.sh
"$HOME/.local/bin/wbot" version
"$HOME/.local/bin/wbot" login
```

版本应为 `wbot 0.2.1` 或更新版本。上面使用安装目录的完整路径，避免终端继续调用其他位置的旧文件。使用自定义配置时，在命令前添加 `--config /absolute/path/to/config.json`。

如果 wbot 后台服务已经运行，需要先停止同一配置的接收进程，再执行登录；如果此前卡在二维码生成阶段、尚未启动后台服务，则无需额外停止进程。

## 更新后仍然失败

请保留完整的错误信息、版本号，以及出错的请求方式和接口位置。0.2.1 的错误会区分初始 POST 和后续 GET 兼容请求，同时省略查询参数和响应正文，避免泄露二维码会话、验证码或凭证。

在出错的虚拟机内核对是否存在 `HTTP_PROXY`、`HTTPS_PROXY`、`ALL_PROXY` 或平台出口网关，并使用同一网络分别检查二维码 POST 和 GET 的状态码。诊断记录只保留状态码、`Allow` 响应头、内容类型及二维码字段是否存在；不要公开完整二维码会话、token、代理密码或验证码。两个请求都失败时，应继续检查实际返回 405 的服务，重复安装 Go 无法解决请求方式被拒绝的问题。

本地模拟测试验证程序逻辑。成功生成二维码验证登录入口可达。用户扫码并完成确认后，才能验证真实账号登录；实际微信收发还需要另外验证。
