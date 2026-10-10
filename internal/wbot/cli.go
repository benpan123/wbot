package wbot

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mdp/qrterminal/v3"
)

const help = `wbot — 通过微信联系你的智能助手

用法：wbot [--config 配置路径] <命令> [选项]

  init    --webhook URL        创建配置
  login                       微信扫码登录
  run                         启动消息接收和本地回复接口
  send    [--to 用户ID] --text 内容
          [--image 图片 | --file 文件 | --video 视频]
  status                      查看运行状态和交付计数
  health                      本机健康检查，不调用 Agent
  version                     查看版本

文字和附件可以一起发送。省略 --to 时回复扫码登录的用户。
回复前需要启动 wbot run，并从微信发送过至少一条消息。
`

func CLI(ctx context.Context, args []string, input io.Reader, output, diagnostics io.Writer) error {
	global := flag.NewFlagSet("wbot", flag.ContinueOnError)
	global.SetOutput(diagnostics)
	configPath := global.String("config", ConfigPath(), "配置路径")
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(output, help)
			return nil
		}
		return err
	}
	args = global.Args()
	if len(args) == 0 || args[0] == "help" {
		fmt.Fprint(output, help)
		return nil
	}
	if args[0] == "version" {
		fmt.Fprintln(output, "wbot", Version)
		return nil
	}
	path, err := filepath.Abs(*configPath)
	if err != nil {
		return err
	}
	switch args[0] {
	case "init":
		flags := flag.NewFlagSet("init", flag.ContinueOnError)
		flags.SetOutput(diagnostics)
		webhook := flags.String("webhook", "http://127.0.0.1:8787/events", "Agent 的 Webhook 地址")
		if err = flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("init 存在无法识别的参数")
		}
		if err = ValidateWebhook(*webhook); err != nil {
			return err
		}
		if err = privateDir(filepath.Dir(path)); err != nil {
			return err
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			return errors.New("配置已存在，已保留原配置")
		}
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(file)
		encoder.SetIndent("", "  ")
		err = encoder.Encode(NewSettings(path, *webhook))
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		fmt.Fprintln(output, "配置已创建：", path)
		fmt.Fprintln(output, "下一步：执行 wbot login，使用微信扫码。")
		return nil
	}
	c, err := LoadSettings(path)
	if err != nil {
		return err
	}
	switch args[0] {
	case "login":
		if len(args) != 1 {
			return errors.New("login 不接受额外参数")
		}
		release, err := AcquireProfile(c.StateDir)
		if err != nil {
			return err
		}
		defer release()
		journal, err := OpenJournal(c.StateDir)
		if err != nil {
			return err
		}
		if len(journal.Read().Pending) > 0 {
			return errors.New("还有尚未送达 Agent 的消息，请先执行 wbot run 完成转发，再重新登录")
		}
		reader := bufio.NewReader(input)
		credentials, err := Authenticate(ctx, NewClient(Credentials{}), journal.Read().Credentials.Token, func(q QRCode) error {
			if strings.HasPrefix(q.Content, "data:image/") {
				comma := strings.IndexByte(q.Content, ',')
				if comma < 0 {
					return errors.New("二维码图片格式无效")
				}
				image, err := base64.StdEncoding.DecodeString(q.Content[comma+1:])
				if err != nil {
					return err
				}
				path := filepath.Join(c.StateDir, "login-qr.png")
				if err = os.WriteFile(path, image, 0600); err != nil {
					return err
				}
				fmt.Fprintln(output, "请打开二维码图片并使用微信扫描：", path)
			} else {
				fmt.Fprintln(output, "请使用微信扫描二维码，并在手机上确认：")
				qrterminal.GenerateHalfBlock(q.Content, qrterminal.L, output)
				fmt.Fprintln(output, "登录链接：", q.Content)
			}
			return nil
		}, func(ctx context.Context) (string, error) {
			fmt.Fprint(output, "请输入微信显示的验证码：")
			type answer struct {
				text string
				err  error
			}
			ready := make(chan answer, 1)
			go func() { text, err := reader.ReadString('\n'); ready <- answer{text, err} }()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case result := <-ready:
				return result.text, result.err
			}
		})
		if err != nil {
			return err
		}
		if err = journal.Update(func(s *Snapshot) {
			*s = Snapshot{Credentials: credentials, ReplyTokens: map[string]string{}, Pending: []Event{}, Delivered: []string{}}
		}); err != nil {
			return err
		}
		_ = os.Remove(filepath.Join(c.StateDir, "login-qr.png"))
		fmt.Fprintln(output, "微信登录成功。执行 wbot run 后，从微信给 ClawBot 发送第一条消息。")
		return nil
	case "run":
		if len(args) != 1 {
			return errors.New("run 不接受额外参数")
		}
		return runProfile(ctx, c, output)
	case "health":
		if len(args) != 1 {
			return errors.New("health 不接受额外参数")
		}
		check, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		result, err := localCall(check, c, http.MethodGet, "/v1/health", nil)
		if err != nil {
			return err
		}
		fmt.Fprintln(output, string(result))
		return nil
	case "status":
		if len(args) != 1 {
			return errors.New("status 不接受额外参数")
		}
		result, err := localCall(ctx, c, http.MethodGet, "/v1/status", nil)
		if errors.Is(err, errOffline) {
			journal, readErr := OpenJournal(c.StateDir)
			if readErr != nil {
				return readErr
			}
			status := NewRelay(c, journal).Status()
			status["running"] = false
			return json.NewEncoder(output).Encode(status)
		}
		if err != nil {
			return err
		}
		fmt.Fprintln(output, string(result))
		return nil
	case "send":
		flags := flag.NewFlagSet("send", flag.ContinueOnError)
		flags.SetOutput(diagnostics)
		to := flags.String("to", "", "目标微信用户")
		text := flags.String("text", "", "回复文字")
		image := flags.String("image", "", "图片路径")
		file := flags.String("file", "", "文件路径")
		video := flags.String("video", "", "视频路径")
		if err = flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("send 存在无法识别的参数")
		}
		request := SendRequest{To: *to, Text: *text}
		for kind, path := range map[string]string{"image": *image, "file": *file, "video": *video} {
			if path != "" {
				if request.File != "" {
					return errors.New("每次只能发送一个附件")
				}
				absolute, err := filepath.Abs(path)
				if err != nil {
					return err
				}
				request.File = absolute
				request.Kind = kind
			}
		}
		if strings.TrimSpace(request.Text) == "" && request.File == "" {
			return errors.New("请提供 --text 或附件参数")
		}
		result, err := localCall(ctx, c, http.MethodPost, "/v1/send", request)
		if err != nil {
			return err
		}
		fmt.Fprintln(output, string(result))
		return nil
	default:
		return fmt.Errorf("无法识别的命令 %q；执行 wbot help 查看用法", args[0])
	}
}

func runProfile(ctx context.Context, c Settings, output io.Writer) error {
	if err := c.ValidateDelivery(); err != nil {
		return err
	}
	release, err := AcquireProfile(c.StateDir)
	if err != nil {
		return err
	}
	defer release()
	journal, err := OpenJournal(c.StateDir)
	if err != nil {
		return err
	}
	if journal.Read().Credentials.Token == "" {
		return errors.New("请先执行 wbot login")
	}
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return fmt.Errorf("本地接口启动失败：%w", err)
	}
	defer listener.Close()
	relay := NewRelay(c, journal)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 2)
	go func() { done <- relay.Serve(ctx, listener) }()
	go func() { done <- relay.Run(ctx) }()
	fmt.Fprintln(output, "wbot 已启动，本地接口：", listener.Addr().String())
	first := <-done
	cancel()
	second := <-done
	if first != nil && !errors.Is(first, context.Canceled) {
		return first
	}
	if second != nil && !errors.Is(second, context.Canceled) {
		return second
	}
	return nil
}

var errOffline = errors.New("wbot 尚未运行，请先执行 wbot run")

func localCall(ctx context.Context, c Settings, method, path string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://"+c.Listen+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.LocalToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := httpClient(3 * time.Minute).Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errOffline
	}
	defer response.Body.Close()
	result, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != 200 {
		var problem struct {
			Error string   `json:"error"`
			IDs   []string `json:"sent_message_ids"`
		}
		if json.Unmarshal(result, &problem) == nil && problem.Error != "" {
			if len(problem.IDs) > 0 {
				return nil, fmt.Errorf("%s；已有 %d 条消息确认发送，请勿直接重复整次发送", problem.Error, len(problem.IDs))
			}
			return nil, errors.New(problem.Error)
		}
		return nil, fmt.Errorf("本地接口 HTTP 状态码 %d", response.StatusCode)
	}
	return result, nil
}
