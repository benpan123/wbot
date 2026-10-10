package wbot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

func (c Settings) ValidateDelivery() error {
	if len(c.OnMessage) == 0 {
		return ValidateWebhook(c.Webhook.URL)
	}
	if c.Webhook.URL != "" {
		return errors.New("on_message 与 webhook.url 只能配置一种交付方式")
	}
	program := c.OnMessage[0]
	if !filepath.IsAbs(program) {
		return errors.New("on_message 的第一个参数需要使用脚本或程序的绝对路径")
	}
	stat, err := os.Stat(program)
	if err != nil || !stat.Mode().IsRegular() || stat.Mode().Perm()&0111 == 0 {
		return errors.New("on_message 指定的程序不存在或没有执行权限")
	}
	return nil
}

// Only a trusted configuration selects executable/arguments. Message contents
// are passed as JSON on stdin and never interpolated into a shell command.
func (r *Relay) runHook(ctx context.Context, event Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	args := r.Config.OnMessage
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	command.Stdin = bytes.NewReader(data)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	command.Env = append(os.Environ(), "WBOT_EVENT_ID="+event.ID, "WBOT_STATE_DIR="+r.Config.StateDir)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = time.Second
	if err = command.Run(); err != nil {
		if ctx.Err() != nil {
			return errors.New("消息脚本已超时或取消，消息已保留")
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return fmt.Errorf("消息脚本退出码 %d，消息已保留", exit.ExitCode())
		}
		return errors.New("消息脚本执行失败，消息已保留")
	}
	return nil
}
