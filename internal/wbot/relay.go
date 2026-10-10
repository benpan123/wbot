package wbot

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Event struct {
	Schema      string       `json:"schema"`
	ID          string       `json:"id"`
	Bot         string       `json:"bot_id"`
	Sender      string       `json:"sender_id"`
	MessageID   string       `json:"message_id"`
	TimestampMS int64        `json:"timestamp_ms"`
	ReceivedAt  string       `json:"received_at"`
	Text        string       `json:"text"`
	VoiceText   string       `json:"voice_text,omitempty"`
	Attachments []Attachment `json:"attachments"`
	CanReply    bool         `json:"can_reply"`
}

type Relay struct {
	Config     Settings
	Journal    *Journal
	WeChat     *Client
	HookHTTP   *http.Client
	Log        *slog.Logger
	sendLock   sync.Mutex
	statusLock sync.RWMutex
	lastIssue  string
	activity   Activity
}

func NewRelay(c Settings, j *Journal) *Relay {
	return &Relay{Config: c, Journal: j, WeChat: NewClient(j.Read().Credentials), HookHTTP: httpClient(15 * time.Second), Log: slog.Default(), activity: Activity{StartedAt: time.Now().UTC()}}
}

func (r *Relay) allowed(id string) bool {
	if id == "" {
		return false
	}
	if len(r.Config.AllowUsers) == 0 {
		_, _, owner, _ := r.Journal.Summary()
		return id == owner
	}
	return slices.Contains(r.Config.AllowUsers, id)
}

func incomingID(message Incoming, bot string) (string, string) {
	id := strings.Trim(string(message.ID), "\"")
	if id == "null" {
		id = ""
	}
	identity := bot + "\x00" + message.Sender + "\x00" + id
	if id == "" {
		message.Context = ""
		encoded, _ := json.Marshal(message)
		identity = bot + "\x00" + string(encoded)
	}
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:]), id
}

// Ingest checkpoints the cursor together with queued events. Network delivery
// happens later, so terminating a process cannot silently discard a received batch.
func (r *Relay) Ingest(ctx context.Context, batch Updates) error {
	snapshot := r.Journal.Read()
	if len(batch.Messages) == 0 && (batch.Cursor == "" || batch.Cursor == snapshot.Cursor) {
		return nil
	}
	known := map[string]bool{}
	for _, id := range snapshot.Delivered {
		known[id] = true
	}
	for _, event := range snapshot.Pending {
		known[event.ID] = true
	}
	events := []Event{}
	contacts := map[string]string{}
	for _, message := range batch.Messages {
		if message.Kind != 1 || message.State == 1 || message.Group != "" || !r.allowed(message.Sender) {
			continue
		}
		if message.Context != "" {
			contacts[message.Sender] = message.Context
		}
		id, messageID := incomingID(message, snapshot.Credentials.Bot)
		if known[id] {
			continue
		}
		known[id] = true
		event := Event{Schema: "wbot.event.v1", ID: id, Bot: snapshot.Credentials.Bot, Sender: message.Sender, MessageID: messageID, TimestampMS: message.Created, ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano), Attachments: []Attachment{}, CanReply: message.Context != "" || snapshot.ReplyTokens[message.Sender] != ""}
		var texts, voices []string
		for index, item := range message.Items {
			if item.Type == 1 && item.Text != nil && item.Text.Text != "" {
				texts = append(texts, item.Text.Text)
			}
			if item.Type == 3 && item.Voice != nil && item.Voice.Text != "" {
				voices = append(voices, item.Voice.Text)
			}
			kind, source := attachmentSource(item)
			if kind != "" && source != nil {
				r.progress()
				event.Attachments = append(event.Attachments, r.WeChat.SaveAttachment(ctx, filepath.Join(r.Config.StateDir, "media"), id, index, item))
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		event.VoiceText = strings.Join(voices, "\n")
		event.Text = strings.Join(texts, "\n")
		if event.Text == "" {
			event.Text = event.VoiceText
		}
		// Unknown message kinds are not silently reinterpreted as text.
		if event.Text == "" && len(event.Attachments) == 0 {
			continue
		}
		events = append(events, event)
	}
	err := r.Journal.Update(func(s *Snapshot) {
		for user, token := range contacts {
			s.ReplyTokens[user] = token
		}
		s.Pending = append(s.Pending, events...)
		if batch.Cursor != "" {
			s.Cursor = batch.Cursor
		}
	})
	if err == nil {
		r.statusLock.Lock()
		r.activity.ReceivedEvents += uint64(len(events))
		r.statusLock.Unlock()
	}
	return err
}

func Signature(key, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func (r *Relay) deliver(ctx context.Context, event Event) error {
	r.statusLock.Lock()
	r.activity.DeliveryAttempts++
	r.statusLock.Unlock()
	if len(r.Config.OnMessage) > 0 {
		return r.runHook(ctx, event)
	}
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.Config.Webhook.URL, bytes.NewReader(body))
	if err != nil {
		return errors.New("Webhook 地址无效")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "wbot/"+Version)
	request.Header.Set("Idempotency-Key", event.ID)
	request.Header.Set("X-Wbot-Event", "message")
	if r.Config.Webhook.Token != "" {
		request.Header.Set("Authorization", "Bearer "+r.Config.Webhook.Token)
	}
	if r.Config.Webhook.SigningKey != "" {
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		request.Header.Set("X-Wbot-Timestamp", timestamp)
		request.Header.Set("X-Wbot-Signature", Signature(r.Config.Webhook.SigningKey, timestamp, body))
	}
	response, err := r.HookHTTP.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Agent Webhook 连接失败，消息已保留")
	}
	defer response.Body.Close()
	io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Agent Webhook 返回 HTTP %d，消息已保留", response.StatusCode)
	}
	return nil
}

func (r *Relay) DeliverOne(ctx context.Context) (bool, error) {
	pending := r.Journal.Read().Pending
	if len(pending) == 0 {
		return false, nil
	}
	event := pending[0]
	if r.allowed(event.Sender) {
		if err := r.deliver(ctx, event); err != nil {
			return true, err
		}
		r.statusLock.Lock()
		r.activity.DeliveredEvents++
		r.activity.LastDeliveryAt = time.Now().UTC()
		r.statusLock.Unlock()
	}
	err := r.Journal.Update(func(s *Snapshot) {
		s.Pending = s.Pending[1:]
		s.Delivered = append(s.Delivered, event.ID)
		if len(s.Delivered) > 2048 {
			s.Delivered = s.Delivered[len(s.Delivered)-2048:]
		}
	})
	return true, err
}

func (r *Relay) setIssue(err error) {
	issue := ""
	if err != nil {
		issue = err.Error()
	}
	r.statusLock.Lock()
	r.lastIssue = issue
	r.statusLock.Unlock()
}

func (r *Relay) Run(ctx context.Context) error {
	if r.WeChat.Token == "" {
		return errors.New("请先执行 wbot login 完成微信登录")
	}
	if err := r.Config.ValidateDelivery(); err != nil {
		return err
	}
	if err := r.WeChat.Notify(ctx, true); err != nil {
		r.Log.Warn("微信启动通知未确认", "error", err)
	}
	defer func() { _ = r.WeChat.Notify(context.Background(), false) }()
	backoff := time.Second
	pollTimeout := 40 * time.Second
	for ctx.Err() == nil {
		r.progress()
		delivered, err := r.DeliverOne(ctx)
		if err == nil && delivered {
			backoff = time.Second
			r.setIssue(nil)
			continue
		}
		if err == nil {
			var batch Updates
			batch, err = r.WeChat.Receive(ctx, r.Journal.Read().Cursor, pollTimeout)
			if err == nil {
				r.statusLock.Lock()
				r.activity.LastReceiveAt = time.Now().UTC()
				if len(batch.Messages) == 0 {
					r.activity.EmptyPolls++
				}
				r.statusLock.Unlock()
				if batch.TimeoutMS > 0 {
					pollTimeout = time.Duration(batch.TimeoutMS)*time.Millisecond + 5*time.Second
					if pollTimeout > 40*time.Second {
						pollTimeout = 40 * time.Second
					}
				}
				err = r.Ingest(ctx, batch)
				if err == nil && len(batch.Messages) == 0 {
					if err = wait(ctx, 250*time.Millisecond); err != nil {
						break
					}
				}
			}
		}
		if ctx.Err() != nil {
			break
		}
		if errors.Is(err, ErrLoginExpired) {
			r.setIssue(err)
			return err
		}
		if err != nil {
			r.setIssue(err)
			r.Log.Warn("稍后重试", "error", err, "delay", backoff.String())
			if err = wait(ctx, backoff); err != nil {
				break
			}
			backoff = min(30*time.Second, backoff*2)
		} else {
			backoff = time.Second
			r.setIssue(nil)
		}
	}
	return ctx.Err()
}

func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type SendRequest struct {
	To   string `json:"to"`
	Text string `json:"text,omitempty"`
	File string `json:"file,omitempty"`
	Kind string `json:"kind,omitempty"`
}

type SendResult struct {
	IDs []string `json:"message_ids"`
}

func (r *Relay) Send(ctx context.Context, request SendRequest) (SendResult, error) {
	r.sendLock.Lock()
	defer r.sendLock.Unlock()
	result := SendResult{IDs: []string{}}
	snapshot := r.Journal.Read()
	if request.To == "" {
		request.To = snapshot.Credentials.Owner
	}
	if !r.allowed(request.To) {
		return result, errors.New("目标用户未获得授权")
	}
	token := snapshot.ReplyTokens[request.To]
	if token == "" {
		return result, errors.New("请先从该微信账号发送一条消息，再进行回复")
	}
	if !utf8.ValidString(request.Text) || len(request.Text) > 64000 {
		return result, errors.New("回复需要使用有效 UTF-8，且不超过 64000 字节")
	}
	if strings.TrimSpace(request.Text) == "" && request.File == "" {
		return result, errors.New("回复内容不能为空")
	}
	var attachment any
	if request.File != "" {
		var err error
		attachment, err = r.WeChat.Upload(ctx, request.To, request.Kind, request.File)
		if err != nil {
			return result, err
		}
	}
	for _, part := range textChunks(request.Text, 2000) {
		id, err := r.WeChat.SendItems(ctx, request.To, token, []any{map[string]any{"type": 1, "text_item": map[string]string{"text": part}}})
		if err != nil {
			return result, err
		}
		result.IDs = append(result.IDs, id)
	}
	if attachment != nil {
		id, err := r.WeChat.SendItems(ctx, request.To, token, []any{attachment})
		if err != nil {
			return result, err
		}
		result.IDs = append(result.IDs, id)
	}
	return result, nil
}

func textChunks(text string, limit int) []string {
	var chunks []string
	for len(text) > 0 {
		cut := min(limit, len(text))
		for cut > 0 && cut < len(text) && !utf8.RuneStart(text[cut]) {
			cut--
		}
		if cut == 0 {
			_, cut = utf8.DecodeRuneInString(text)
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}
	return chunks
}

func (r *Relay) Status() map[string]any {
	loggedIn, bot, owner, pending := r.Journal.Summary()
	r.statusLock.RLock()
	issue, activity := r.lastIssue, r.activity
	r.statusLock.RUnlock()
	return map[string]any{"version": Version, "running": true, "logged_in": loggedIn, "bot_id": bot, "owner_id": owner, "pending_messages": pending, "last_issue": issue, "activity": activity}
}
