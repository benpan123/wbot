package wbot

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const Version = "0.2.0"
const Gateway = "https://ilinkai.weixin.qq.com"

// Settings belong to one WeChat login. Independent profiles use separate files.
type Settings struct {
	Listen     string       `json:"listen"`
	LocalToken string       `json:"local_token"`
	StateDir   string       `json:"state_dir"`
	AllowUsers []string     `json:"allow_users"`
	Webhook    HookSettings `json:"webhook"`
	OnMessage  []string     `json:"on_message,omitempty"`
}

type HookSettings struct {
	URL        string `json:"url"`
	Token      string `json:"token,omitempty"`
	SigningKey string `json:"signing_key,omitempty"`
}

func ConfigPath() string {
	base, err := os.UserConfigDir()
	if err != nil {
		base = "."
	}
	// A consistent path across macOS and Linux is easier to document and back up.
	if home, err := os.UserHomeDir(); err == nil {
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "wbot", "config.json")
}

func NewSettings(path, webhook string) Settings {
	return Settings{Listen: "127.0.0.1:18791", LocalToken: randomID(), StateDir: filepath.Join(filepath.Dir(path), "state"), AllowUsers: []string{}, Webhook: HookSettings{URL: webhook}}
}

func LoadSettings(path string) (Settings, error) {
	var c Settings
	data, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("读取配置失败，请先执行 wbot init：%w", err)
	}
	if err = json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("配置格式错误：%w", err)
	}
	if c.StateDir == "" {
		c.StateDir = filepath.Join(filepath.Dir(path), "state")
	}
	if !filepath.IsAbs(c.StateDir) {
		c.StateDir = filepath.Join(filepath.Dir(path), c.StateDir)
	}
	c.StateDir, err = filepath.Abs(c.StateDir)
	if err != nil {
		return c, err
	}
	if err = validateListen(c.Listen); err != nil {
		return c, err
	}
	if c.LocalToken, err = secret(c.LocalToken); err != nil {
		return c, err
	}
	if len(c.LocalToken) < 24 {
		return c, errors.New("local_token 至少需要 24 个字符")
	}
	if c.Webhook.Token, err = secret(c.Webhook.Token); err != nil {
		return c, err
	}
	if c.Webhook.SigningKey, err = secret(c.Webhook.SigningKey); err != nil {
		return c, err
	}
	return c, nil
}

func secret(s string) (string, error) {
	if !strings.HasPrefix(s, "env:") {
		return s, nil
	}
	name := strings.TrimPrefix(s, "env:")
	value := os.Getenv(name)
	if value == "" {
		return "", fmt.Errorf("环境变量 %s 尚未设置", name)
	}
	return value, nil
}

func validateListen(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("listen 需要使用 127.0.0.1:端口 或 [::1]:端口")
	}
	ip := net.ParseIP(host)
	n, e := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || e != nil || n < 0 || n > 65535 {
		return errors.New("本地接口必须使用回环 IP 和有效端口")
	}
	return nil
}

func ValidateWebhook(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("请配置有效的 webhook.url")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && loopbackHost(u.Hostname()) {
		return nil
	}
	return errors.New("Webhook 需要使用 HTTPS；本机回环地址可以使用 HTTP")
}

func loopbackHost(s string) bool {
	if s == "localhost" {
		return true
	}
	ip := net.ParseIP(s)
	return ip != nil && ip.IsLoopback()
}

func validateGateway(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("微信接口地址无效")
	}
	if u.Scheme == "https" && (u.Hostname() == "weixin.qq.com" || strings.HasSuffix(u.Hostname(), ".weixin.qq.com")) {
		return nil
	}
	// Local protocol simulators are useful without real credentials.
	if u.Scheme == "http" && loopbackHost(u.Hostname()) {
		return nil
	}
	return errors.New("微信接口地址必须属于腾讯微信 HTTPS 域名")
}

func randomID() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return nil
}

// writeJSON replaces state only after a complete, flushed write.
func writeJSON(path string, value any) error {
	if err := privateDir(filepath.Dir(path)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".wbot-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
