package wbot

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Authenticate exposes display and verification callbacks, keeping the protocol
// independent of a terminal and allowing the entire login flow to be simulated.
func Authenticate(ctx context.Context, client *Client, previous string, display func(QRCode) error, verify func(context.Context) (string, error)) (Credentials, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	originalEndpoint := client.Endpoint
	for refresh := 0; refresh < 3; refresh++ {
		client.Endpoint = originalEndpoint
		qr, err := client.QR(ctx, previous)
		if err != nil {
			return Credentials{}, err
		}
		if err = display(qr); err != nil {
			return Credentials{}, err
		}
		code := ""
		failures := 0
		for {
			status, err := client.LoginStatus(ctx, qr.ID, code)
			if err != nil {
				if ctx.Err() != nil {
					return Credentials{}, ctx.Err()
				}
				failures++
				if failures >= 5 {
					return Credentials{}, err
				}
				if err = wait(ctx, time.Second); err != nil {
					return Credentials{}, err
				}
				continue
			}
			failures = 0
			switch status.Status {
			case "confirmed", "binded_redirect":
				if status.Token == "" || status.Bot == "" || status.Owner == "" || status.Endpoint == "" {
					return Credentials{}, errors.New("登录响应缺少凭证或账号信息，请重新扫码登录")
				}
				if err = validateGateway(status.Endpoint); err != nil {
					return Credentials{}, err
				}
				return Credentials{Token: status.Token, Bot: status.Bot, Owner: status.Owner, Endpoint: strings.TrimRight(status.Endpoint, "/")}, nil
			case "expired":
				goto nextQR
			case "need_verifycode":
				if verify == nil {
					return Credentials{}, errors.New("请在交互终端输入微信显示的验证码")
				}
				code, err = verify(ctx)
				if err != nil {
					return Credentials{}, err
				}
				code = strings.TrimSpace(code)
				if code == "" {
					return Credentials{}, errors.New("验证码不能为空")
				}
			case "verify_code_blocked":
				return Credentials{}, errors.New("验证码尝试次数过多，请稍后重新登录")
			case "scaned_but_redirect":
				if status.Redirect == "" {
					return Credentials{}, errors.New("微信登录缺少重定向地址")
				}
				if err = client.Redirect(status.Redirect); err != nil {
					return Credentials{}, err
				}
			case "wait", "scaned":
			default:
				return Credentials{}, errors.New("微信返回了尚未支持的登录状态")
			}
			if err = wait(ctx, time.Second); err != nil {
				return Credentials{}, err
			}
		}
	nextQR:
	}
	return Credentials{}, errors.New("二维码已连续过期，请重新执行 wbot login")
}
