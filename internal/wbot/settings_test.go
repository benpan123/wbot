package wbot

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitAndProfileIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile", "config.json")
	var output bytes.Buffer
	args := []string{"--config", path, "init", "--webhook", "http://127.0.0.1:8787/events"}
	if err := CLI(context.Background(), args, strings.NewReader(""), &output, &output); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err := CLI(context.Background(), args, strings.NewReader(""), &output, &output); err == nil {
		t.Fatal("init replaced an existing profile")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("init changed config")
	}
	c, err := LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.LocalToken) != 48 || !filepath.IsAbs(c.StateDir) {
		t.Fatal("invalid generated configuration")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("config is not private")
	}
	release, err := AcquireProfile(c.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = AcquireProfile(c.StateDir); err == nil {
		t.Fatal("concurrent owner accepted")
	}
	release()
	release, err = AcquireProfile(c.StateDir)
	if err != nil {
		t.Fatal("released lock remained held")
	}
	release()
}

func TestEndpointValidationAndSecrets(t *testing.T) {
	for _, host := range []string{"0.0.0.0:18791", ":18791", "example.com:80", "127.0.0.1:-1"} {
		if validateListen(host) == nil {
			t.Fatalf("unsafe listen accepted: %s", host)
		}
	}
	for _, host := range []string{"127.0.0.1:18791", "[::1]:18791"} {
		if err := validateListen(host); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{"http://example.com/hook", "https://user:secret@example.com/", "", "file:///tmp/key"} {
		if ValidateWebhook(raw) == nil {
			t.Fatalf("unsafe hook accepted: %s", raw)
		}
	}
	if err := validateGateway("https://ilinkai.weixin.qq.com.attacker.example"); err == nil {
		t.Fatal("credential redirect accepted")
	}
	t.Setenv("WBOT_TEST_SECRET", "sensitive-value")
	got, err := secret("env:WBOT_TEST_SECRET")
	if err != nil || got != "sensitive-value" {
		t.Fatal("env reference did not resolve")
	}
	if _, err = secret("env:WBOT_TEST_MISSING"); err == nil {
		t.Fatal("missing variable accepted")
	}
}

func TestFailedStateWriteDoesNotChangeMemory(t *testing.T) {
	j, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = j.Update(func(s *Snapshot) { s.Cursor = "before" }); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "regular-file")
	if err = os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	j.path = filepath.Join(file, "impossible", "session.json")
	if err = j.Update(func(s *Snapshot) { s.Cursor = "after" }); err == nil {
		t.Fatal("write unexpectedly succeeded")
	}
	if j.Read().Cursor != "before" {
		t.Fatal("memory advanced beyond durable state")
	}
}

func TestRedactedOfflineStatus(t *testing.T) {
	relay := testRelay(t, "http://127.0.0.1")
	configPath := filepath.Join(t.TempDir(), "config.json")
	relay.Config.Listen = "127.0.0.1:1"
	if err := writeJSON(configPath, relay.Config); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := CLI(context.Background(), []string{"--config", configPath, "status"}, strings.NewReader(""), &output, &output); err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	if err := json.Unmarshal(output.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status["running"] != false || status["logged_in"] != true {
		t.Fatal(status)
	}
	if strings.Contains(output.String(), "private-bot-token") {
		t.Fatal("offline status leaked token")
	}
}
