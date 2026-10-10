package wbot

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var ErrLoginExpired = errors.New("微信登录已过期，请重新执行 wbot login")

type APIError struct {
	Code      int
	Operation string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("微信接口 %s 返回错误码 %d", e.Operation, e.Code)
}

type Client struct {
	Endpoint string
	Token    string
	HTTP     *http.Client
}

func NewClient(c Credentials) *Client {
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = Gateway
	}
	return &Client{Endpoint: strings.TrimRight(endpoint, "/"), Token: c.Token, HTTP: httpClient(45 * time.Second)}
}

func httpClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}

func (c *Client) call(ctx context.Context, method, operation string, body map[string]any, out any, authenticated bool) error {
	if err := validateGateway(c.Endpoint); err != nil {
		return err
	}
	if authenticated {
		if c.Token == "" {
			return errors.New("尚未登录微信，请执行 wbot login")
		}
		body["base_info"] = map[string]string{"channel_version": Version, "bot_agent": "wbot/" + Version}
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.Endpoint+"/ilink/bot/"+operation, reader)
	if err != nil {
		return errors.New("无法创建微信接口请求")
	}
	request.Header.Set("iLink-App-Id", "bot")
	request.Header.Set("iLink-App-ClientVersion", "512")
	if method == http.MethodPost {
		var nonce [4]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("AuthorizationType", "ilink_bot_token")
		request.Header.Set("X-WECHAT-UIN", base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(uint64(binary.BigEndian.Uint32(nonce[:])), 10))))
	}
	if authenticated {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("微信接口连接失败，请检查网络后重试")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("微信接口 HTTP 状态码 %d", response.StatusCode)
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, 8<<20+1))
	if err != nil {
		return errors.New("微信接口响应读取失败")
	}
	if len(encoded) > 8<<20 {
		return errors.New("微信接口响应过大")
	}
	var result struct {
		Ret  int `json:"ret"`
		Code int `json:"errcode"`
	}
	if err := json.Unmarshal(encoded, &result); err != nil {
		return errors.New("微信接口返回了无效 JSON")
	}
	if result.Ret == -14 || result.Code == -14 {
		return ErrLoginExpired
	}
	code := result.Ret
	if code == 0 {
		code = result.Code
	}
	if code != 0 {
		return &APIError{Code: code, Operation: strings.Split(operation, "?")[0]}
	}
	if out != nil {
		return json.Unmarshal(encoded, out)
	}
	return nil
}

type QRCode struct {
	ID      string `json:"qrcode"`
	Content string `json:"qrcode_img_content"`
}
type LoginStatus struct {
	Status   string `json:"status"`
	Token    string `json:"bot_token"`
	Bot      string `json:"ilink_bot_id"`
	Owner    string `json:"ilink_user_id"`
	Endpoint string `json:"baseurl"`
	Redirect string `json:"redirect_host"`
}

func (c *Client) QR(ctx context.Context, previousToken string) (QRCode, error) {
	var q QRCode
	tokens := []string{}
	if previousToken != "" {
		tokens = append(tokens, previousToken)
	}
	err := c.call(ctx, http.MethodPost, "get_bot_qrcode?bot_type=3", map[string]any{"local_token_list": tokens}, &q, false)
	if err == nil && (q.ID == "" || q.Content == "") {
		err = errors.New("微信接口没有返回完整的登录二维码")
	}
	return q, err
}

func (c *Client) LoginStatus(ctx context.Context, id, verify string) (LoginStatus, error) {
	query := url.Values{"qrcode": {id}}
	if verify != "" {
		query.Set("verify_code", verify)
	}
	var s LoginStatus
	err := c.call(ctx, http.MethodGet, "get_qrcode_status?"+query.Encode(), nil, &s, false)
	return s, err
}

func (c *Client) Redirect(host string) error {
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	if err := validateGateway(host); err != nil {
		return err
	}
	c.Endpoint = strings.TrimRight(host, "/")
	return nil
}

type Incoming struct {
	ID        json.RawMessage `json:"message_id"`
	Sequence  json.RawMessage `json:"seq,omitempty"`
	Sender    string          `json:"from_user_id"`
	Recipient string          `json:"to_user_id"`
	Created   int64           `json:"create_time_ms"`
	Kind      int             `json:"message_type"`
	State     int             `json:"message_state"`
	Group     string          `json:"group_id,omitempty"`
	Context   string          `json:"context_token"`
	Items     []Item          `json:"item_list"`
}

type Item struct {
	Type int `json:"type"`
	Text *struct {
		Text string `json:"text"`
	} `json:"text_item,omitempty"`
	Voice *struct {
		Text  string        `json:"text"`
		Media *MediaPointer `json:"media,omitempty"`
	} `json:"voice_item,omitempty"`
	Image *MediaItem `json:"image_item,omitempty"`
	File  *MediaItem `json:"file_item,omitempty"`
	Video *MediaItem `json:"video_item,omitempty"`
}

type Updates struct {
	Messages  []Incoming `json:"msgs"`
	Cursor    string     `json:"get_updates_buf"`
	TimeoutMS int        `json:"longpolling_timeout_ms"`
}

func (c *Client) Receive(ctx context.Context, cursor string, timeout time.Duration) (Updates, error) {
	var updates Updates
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := c.call(ctx, http.MethodPost, "getupdates", map[string]any{"get_updates_buf": cursor}, &updates, true)
	return updates, err
}

func (c *Client) SendItems(ctx context.Context, to, replyToken string, items []any) (string, error) {
	id := "wbot-" + randomID()
	message := map[string]any{
		"from_user_id": "", "to_user_id": to, "client_id": id,
		"message_type": 2, "message_state": 2, "context_token": replyToken, "item_list": items,
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	err := c.call(ctx, http.MethodPost, "sendmessage", map[string]any{"msg": message}, nil, true)
	return id, err
}

func (c *Client) Notify(ctx context.Context, starting bool) error {
	operation := "msg/notifystop"
	if starting {
		operation = "msg/notifystart"
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return c.call(ctx, http.MethodPost, operation, map[string]any{}, nil, true)
}
