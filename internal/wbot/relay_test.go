package wbot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func testRelay(t *testing.T, endpoint string) *Relay {
	t.Helper()
	dir := t.TempDir()
	c := NewSettings(dir+"/config.json", "http://127.0.0.1:8787/events")
	j, err := OpenJournal(c.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err = j.Update(func(s *Snapshot) {
		s.Credentials = Credentials{Token: "private-bot-token", Bot: "bot-one", Owner: "owner-one", Endpoint: endpoint}
	}); err != nil {
		t.Fatal(err)
	}
	relay := NewRelay(c, j)
	relay.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return relay
}

func batch(t *testing.T, raw string) Updates {
	t.Helper()
	var update Updates
	if err := json.Unmarshal([]byte(raw), &update); err != nil {
		t.Fatal(err)
	}
	return update
}

const ownerMessage = `{"msgs":[{"message_id":9007199254740993,"from_user_id":"owner-one","to_user_id":"bot-one","message_type":1,"message_state":2,"context_token":"private-reply-token","item_list":[{"type":1,"text_item":{"text":"你好"}}]}],"get_updates_buf":"cursor-next"}`

func TestDurableDeliveryAndReplies(t *testing.T) {
	var calls atomic.Int32
	var sent atomic.Int32
	var eventID string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		switch q.URL.Path {
		case "/events":
			data, _ := io.ReadAll(q.Body)
			if strings.Contains(string(data), "private-") {
				t.Error("webhook leaked a WeChat credential")
			}
			var event Event
			if err := json.Unmarshal(data, &event); err != nil {
				t.Error(err)
			}
			if event.MessageID != "9007199254740993" || event.Text != "你好" || !event.CanReply {
				t.Errorf("invalid event: %+v", event)
			}
			if q.Header.Get("Authorization") != "Bearer hook-key" {
				t.Error("webhook authentication missing")
			}
			if q.Header.Get("X-Wbot-Signature") != Signature("signing-key", q.Header.Get("X-Wbot-Timestamp"), data) {
				t.Error("webhook signature invalid")
			}
			if calls.Add(1) == 1 {
				eventID = event.ID
				w.WriteHeader(503)
				return
			}
			if event.ID != eventID || q.Header.Get("Idempotency-Key") != eventID {
				t.Error("retry changed event identity")
			}
			w.WriteHeader(204)
		case "/ilink/bot/sendmessage":
			if q.Header.Get("Authorization") != "Bearer private-bot-token" {
				t.Error("bot authentication missing")
			}
			var body struct {
				Msg struct {
					To       string `json:"to_user_id"`
					Token    string `json:"context_token"`
					ClientID string `json:"client_id"`
					Items    []Item `json:"item_list"`
				} `json:"msg"`
			}
			if err := json.NewDecoder(q.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.Msg.To != "owner-one" || body.Msg.Token != "private-reply-token" || body.Msg.ClientID == "" || body.Msg.Items[0].Text.Text != "收到" {
				t.Errorf("wrong reply: %+v", body)
			}
			sent.Add(1)
			io.WriteString(w, `{"ret":0}`)
		default:
			t.Errorf("unexpected endpoint %s", q.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer fake.Close()
	relay := testRelay(t, fake.URL)
	relay.Config.Webhook = HookSettings{URL: fake.URL + "/events", Token: "hook-key", SigningKey: "signing-key"}
	ctx := context.Background()
	if err := relay.Ingest(ctx, batch(t, ownerMessage)); err != nil {
		t.Fatal(err)
	}
	if relay.Journal.Read().Cursor != "cursor-next" || len(relay.Journal.Read().Pending) != 1 {
		t.Fatal("batch was not checkpointed")
	}
	if _, err := relay.DeliverOne(ctx); err == nil {
		t.Fatal("503 must preserve the event")
	}
	if len(relay.Journal.Read().Pending) != 1 {
		t.Fatal("failed delivery was discarded")
	}
	// Simulate a process restart before retrying the exact event.
	restarted, err := OpenJournal(relay.Config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	relay.Journal = restarted
	if _, err := relay.DeliverOne(ctx); err != nil {
		t.Fatal(err)
	}
	if err := relay.Ingest(ctx, batch(t, ownerMessage)); err != nil {
		t.Fatal(err)
	}
	if len(relay.Journal.Read().Pending) != 0 {
		t.Fatal("already delivered message was queued again")
	}
	result, err := relay.Send(ctx, SendRequest{Text: "收到"})
	if err != nil {
		t.Fatal(err)
	}
	if sent.Load() != 1 || len(result.IDs) != 1 {
		t.Fatal("reply did not complete")
	}
	if _, err := relay.Send(ctx, SendRequest{To: "stranger", Text: "no"}); err == nil {
		t.Fatal("non-allowlisted recipient accepted")
	}
	info, err := os.Stat(relay.Journal.path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("session file is not private")
	}
}

func TestAllowlistAndVoiceNormalization(t *testing.T) {
	relay := testRelay(t, "http://127.0.0.1")
	raw := `{"msgs":[
 {"message_id":1,"from_user_id":"stranger","message_type":1,"context_token":"stranger-token","item_list":[{"type":1,"text_item":{"text":"ignored"}}]},
 {"message_id":2,"from_user_id":"owner-one","message_type":2,"item_list":[{"type":1,"text_item":{"text":"bot echo"}}]},
 {"message_id":3,"from_user_id":"owner-one","message_type":1,"group_id":"group","item_list":[{"type":1,"text_item":{"text":"group"}}]},
 {"message_id":4,"from_user_id":"owner-one","message_type":1,"item_list":[{"type":3,"voice_item":{"text":"整理会议纪要"}}]}
 ],"get_updates_buf":"c"}`
	if err := relay.Ingest(context.Background(), batch(t, raw)); err != nil {
		t.Fatal(err)
	}
	s := relay.Journal.Read()
	if len(s.Pending) != 1 || s.Pending[0].Text != "整理会议纪要" || s.Pending[0].VoiceText != "整理会议纪要" {
		t.Fatalf("unexpected events: %+v", s.Pending)
	}
	if _, ok := s.ReplyTokens["stranger"]; ok {
		t.Fatal("unauthorized token saved")
	}
	relay.Config.AllowUsers = []string{"different-owner"}
	called, err := relay.DeliverOne(context.Background())
	if !called || err != nil || len(relay.Journal.Read().Pending) != 0 {
		t.Fatal("revoked access was not respected")
	}
}

func TestReceiveCancellationAndSessionExpiry(t *testing.T) {
	started := make(chan struct{})
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		io.Copy(io.Discard, q.Body)
		if strings.Contains(q.URL.Path, "notify") {
			io.WriteString(w, `{"ret":0}`)
			return
		}
		close(started)
		<-q.Context().Done()
	}))
	defer fake.Close()
	relay := testRelay(t, fake.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- relay.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("poll never started")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected shutdown error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("poll did not cancel promptly")
	}
	expired := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		io.WriteString(w, `{"ret":-14,"errmsg":"do not log private token"}`)
	}))
	defer expired.Close()
	_, err := NewClient(Credentials{Endpoint: expired.URL, Token: "token"}).Receive(context.Background(), "", time.Second)
	if !errors.Is(err, ErrLoginExpired) {
		t.Fatal(err)
	}
}

func TestControlAuthenticationAndShutdown(t *testing.T) {
	relay := testRelay(t, "http://127.0.0.1")
	for _, token := range []string{"", "wrong"} {
		q := httptest.NewRequest("GET", "/v1/status", nil)
		q.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		relay.Handler().ServeHTTP(response, q)
		if response.Code != 401 {
			t.Fatalf("unauthenticated request returned %d", response.Code)
		}
	}
	q := httptest.NewRequest("POST", "/v1/send", strings.NewReader(`{"text":"hi","unknown":true}`))
	q.Header.Set("Authorization", "Bearer "+relay.Config.LocalToken)
	q.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	relay.Handler().ServeHTTP(response, q)
	if response.Code != 400 {
		t.Fatalf("unknown field accepted: %d", response.Code)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- relay.Serve(ctx, listener) }()
	request, _ := http.NewRequest("GET", "http://"+listener.Addr().String()+"/v1/status", nil)
	request.Header.Set("Authorization", "Bearer "+relay.Config.LocalToken)
	reply, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(reply.Body)
	reply.Body.Close()
	if strings.Contains(string(data), "private-bot-token") || strings.Contains(string(data), relay.Config.LocalToken) {
		t.Fatal("status leaked credentials")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not stop")
	}
}

func TestTextChunks(t *testing.T) {
	text := strings.Repeat("微信🙂 hello\n", 1000)
	chunks := textChunks(text, 2000)
	if strings.Join(chunks, "") != text {
		t.Fatal("text changed while splitting")
	}
	for _, chunk := range chunks {
		if len(chunk) > 2000 || !utf8.ValidString(chunk) {
			t.Fatal("invalid text chunk")
		}
	}
}
