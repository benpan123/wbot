package wbot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func executable(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "message hook.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func healthRequest(r *Relay, authorized bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	if authorized {
		request.Header.Set("Authorization", "Bearer "+r.Config.LocalToken)
	}
	response := httptest.NewRecorder()
	r.Handler().ServeHTTP(response, request)
	return response
}

// Exercise the real receive loop in both delivery modes: empty polls and
// repeated health/status requests must not invoke the Agent or rewrite state.
func TestIdlePollingDoesNotInvokeAgent(t *testing.T) {
	for _, mode := range []string{"webhook", "script"} {
		t.Run(mode, func(t *testing.T) {
			var hookCalls atomic.Int32
			incoming := make(chan struct{}, 1)
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				io.Copy(io.Discard, q.Body)
				switch {
				case q.URL.Path == "/events":
					hookCalls.Add(1)
					w.WriteHeader(204)
				case strings.HasSuffix(q.URL.Path, "/getupdates"):
					select {
					case <-incoming:
						io.WriteString(w, ownerMessage)
					default:
						io.WriteString(w, `{"ret":0,"msgs":[]}`)
					}
				default:
					io.WriteString(w, `{"ret":0}`)
				}
			}))
			defer fake.Close()
			relay := testRelay(t, fake.URL)
			relay.Config.Webhook.URL = fake.URL + "/events"
			marker := filepath.Join(t.TempDir(), "calls")
			if mode == "script" {
				relay.Config.Webhook.URL = ""
				relay.Config.OnMessage = []string{executable(t, "cat >/dev/null\nprintf '%s\\n' \"$WBOT_EVENT_ID\" >>\"$1\"\n"), marker}
			}
			before, err := os.Stat(relay.Journal.path)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- relay.Run(ctx) }()
			defer func() {
				cancel()
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Errorf("receive loop ended unexpectedly: %v", err)
					}
				case <-time.After(2 * time.Second):
					t.Error("receive loop did not stop")
				}
			}()
			eventually(t, func() bool { return relay.Health().Activity.EmptyPolls >= 3 })
			for range 10 {
				if response := healthRequest(relay, true); response.Code != 200 {
					t.Fatalf("health check failed: %s", response.Body.String())
				}
				_ = relay.Status()
			}
			after, err := os.Stat(relay.Journal.path)
			if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
				t.Fatal("idle polling rewrote the state file")
			}
			if hookCalls.Load() != 0 || relay.Health().Activity.DeliveryAttempts != 0 {
				t.Fatal("idle polling invoked the Agent")
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("idle polling invoked the script")
			}
			incoming <- struct{}{}
			eventually(t, func() bool { return relay.Health().Activity.DeliveredEvents == 1 })
			eventually(t, func() bool { return relay.Health().Activity.EmptyPolls >= 5 })
			activity := relay.Health().Activity
			if activity.ReceivedEvents != 1 || activity.DeliveryAttempts != 1 || activity.DeliveredEvents != 1 {
				t.Fatalf("unexpected delivery counts: %+v", activity)
			}
			if mode == "script" {
				calls, err := os.ReadFile(marker)
				if err != nil || len(strings.Fields(string(calls))) != 1 || hookCalls.Load() != 0 {
					t.Fatal("script delivery did not run exactly once")
				}
			} else if hookCalls.Load() != 1 {
				t.Fatal("webhook delivery did not run exactly once")
			}
		})
	}
}

func TestHealthReportsStartupFailureAndStall(t *testing.T) {
	relay := testRelay(t, "http://127.0.0.1")
	if response := healthRequest(relay, false); response.Code != 401 {
		t.Fatal("health endpoint did not require authentication")
	}
	if response := healthRequest(relay, true); response.Code != 503 {
		t.Fatal("health check succeeded before the first receive")
	}
	relay.activity.LastReceiveAt = time.Now()
	relay.progress()
	if response := healthRequest(relay, true); response.Code != 200 {
		t.Fatal("healthy receiver failed health check")
	}
	relay.setIssue(errors.New("connection failed"))
	if response := healthRequest(relay, true); response.Code != 503 || !strings.Contains(response.Body.String(), "connection failed") {
		t.Fatal("health check hid the delivery failure")
	}
	relay.setIssue(nil)
	relay.activity.LastProgressAt = time.Now().Add(-6 * time.Minute)
	if relay.Health().Healthy {
		t.Fatal("stalled receiver passed health check")
	}
}

func TestMessageHookRetriesAndTreatsTextAsData(t *testing.T) {
	relay := testRelay(t, "http://127.0.0.1")
	dir := t.TempDir()
	payload, calls, success := filepath.Join(dir, "payload"), filepath.Join(dir, "calls"), filepath.Join(dir, "success")
	relay.Config.Webhook.URL = ""
	relay.Config.OnMessage = []string{executable(t, "cat >\"$1\"\nprintf '%s\\n' \"$WBOT_EVENT_ID\" >>\"$2\"\ntest -f \"$3\"\n"), payload, calls, success}
	if err := relay.Config.ValidateDelivery(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	message := batch(t, ownerMessage)
	unwanted := filepath.Join(dir, "must-not-exist")
	message.Messages[0].Items[0].Text.Text = "$(touch " + unwanted + ") ; `id`\n你好"
	if err := relay.Ingest(ctx, message); err != nil {
		t.Fatal(err)
	}
	want := relay.Journal.Read().Pending[0]
	if _, err := relay.DeliverOne(ctx); err == nil || len(relay.Journal.Read().Pending) != 1 {
		t.Fatal("failed script did not retain the message")
	}
	if err := os.WriteFile(success, nil, 0600); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenJournal(relay.Config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	relay.Journal = restarted
	if _, err := relay.DeliverOne(ctx); err != nil || len(relay.Journal.Read().Pending) != 0 {
		t.Fatalf("script retry failed: %v", err)
	}
	data, err := os.ReadFile(payload)
	var got Event
	if err != nil || json.Unmarshal(data, &got) != nil || got.ID != want.ID || got.Text != want.Text {
		t.Fatal("script did not receive the original JSON event")
	}
	ids, err := os.ReadFile(calls)
	if err != nil || string(ids) != want.ID+"\n"+want.ID+"\n" {
		t.Fatal("retry changed event identity")
	}
	if _, err := os.Stat(unwanted); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("message contents were executed as a shell command")
	}
	activity := relay.Health().Activity
	if activity.DeliveryAttempts != 2 || activity.DeliveredEvents != 1 {
		t.Fatalf("retry counters are incorrect: %+v", activity)
	}
}

func TestMessageHookValidationAndCancellation(t *testing.T) {
	relay := testRelay(t, "http://127.0.0.1")
	relay.Config.OnMessage = []string{"relative-script"}
	if relay.Config.ValidateDelivery() == nil {
		t.Fatal("ambiguous webhook and command configuration accepted")
	}
	relay.Config.Webhook.URL = ""
	if relay.Config.ValidateDelivery() == nil {
		t.Fatal("relative executable path accepted")
	}
	marker := filepath.Join(t.TempDir(), "child-finished")
	relay.Config.OnMessage = []string{executable(t, "sleep 1; touch \"$1\"\n"), marker}
	if err := relay.Config.ValidateDelivery(); err != nil {
		t.Fatal(err)
	}
	if err := relay.Ingest(context.Background(), batch(t, ownerMessage)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := relay.DeliverOne(ctx); err == nil || len(relay.Journal.Read().Pending) != 1 {
		t.Fatal("cancelled hook did not preserve the message")
	}
	if time.Since(start) > time.Second {
		t.Fatal("hook cancellation did not stop promptly")
	}
	time.Sleep(time.Second)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled hook left its child running")
	}
}

func TestUnchangedJournalDoesNotRewriteFile(t *testing.T) {
	relay := testRelay(t, "http://127.0.0.1")
	before, err := os.Stat(relay.Journal.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := relay.Journal.Update(func(s *Snapshot) {}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(relay.Journal.path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("unchanged journal rewrote its file")
	}
	if err := relay.Ingest(context.Background(), Updates{Cursor: "new-empty-cursor"}); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenJournal(relay.Config.StateDir)
	if err != nil || reopened.Read().Cursor != "new-empty-cursor" {
		t.Fatal("changed empty-batch cursor was not persisted")
	}
}
