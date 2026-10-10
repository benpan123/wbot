package wbot

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
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
)

func TestQRProtocolAndVerification(t *testing.T) {
	var displayed atomic.Int32
	var checked atomic.Int32
	var address string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Header.Get("iLink-App-Id") != "bot" {
			t.Error("application header missing")
		}
		if q.Header.Get("Authorization") != "" {
			t.Error("QR request included bot authorization")
		}
		switch q.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			if q.Method != "POST" || q.URL.Query().Get("bot_type") != "3" {
				t.Error("incorrect QR method/query")
			}
			var body map[string]any
			json.NewDecoder(q.Body).Decode(&body)
			if _, ok := body["base_info"]; ok {
				t.Error("QR request should omit base_info")
			}
			tokens, ok := body["local_token_list"].([]any)
			if !ok || len(tokens) != 0 {
				t.Error("incorrect local token list")
			}
			if q.Header.Get("X-WECHAT-UIN") == "" || q.Header.Get("AuthorizationType") != "ilink_bot_token" {
				t.Error("QR metadata missing")
			}
			io.WriteString(w, `{"qrcode":"qr-session","qrcode_img_content":"https://example.invalid/qr"}`)
		case "/ilink/bot/get_qrcode_status":
			if q.Method != "GET" || q.URL.Query().Get("qrcode") != "qr-session" {
				t.Error("incorrect QR status request")
			}
			if q.Header.Get("AuthorizationType") != "" || q.Header.Get("X-WECHAT-UIN") != "" {
				t.Error("QR status should omit POST metadata")
			}
			if checked.Add(1) == 1 {
				io.WriteString(w, `{"status":"need_verifycode"}`)
				return
			}
			if q.URL.Query().Get("verify_code") != "2468" {
				t.Error("verification code missing")
			}
			json.NewEncoder(w).Encode(map[string]string{"status": "confirmed", "bot_token": "token", "ilink_bot_id": "bot", "ilink_user_id": "owner", "baseurl": address})
		default:
			w.WriteHeader(404)
		}
	}))
	defer fake.Close()
	address = fake.URL
	client := NewClient(Credentials{Endpoint: fake.URL})
	creds, err := Authenticate(context.Background(), client, "", func(q QRCode) error { displayed.Add(1); return nil }, func(context.Context) (string, error) { return "2468", nil })
	if err != nil {
		t.Fatal(err)
	}
	if creds.Owner != "owner" || creds.Token != "token" || displayed.Load() != 1 {
		t.Fatal("login did not complete")
	}
}

func TestExpiredQRRefreshLimit(t *testing.T) {
	var count atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if strings.Contains(q.URL.Path, "get_bot_qrcode") {
			count.Add(1)
			io.WriteString(w, `{"qrcode":"q","qrcode_img_content":"qr"}`)
			return
		}
		io.WriteString(w, `{"status":"expired"}`)
	}))
	defer fake.Close()
	_, err := Authenticate(context.Background(), NewClient(Credentials{Endpoint: fake.URL}), "", func(QRCode) error { return nil }, nil)
	if err == nil || count.Load() != 3 {
		t.Fatalf("QR retry was unbounded: %d %v", count.Load(), err)
	}
}

func TestQRMethodCompatibility(t *testing.T) {
	for _, test := range []struct {
		name        string
		status      int
		body        string
		wantCalls   int32
		wantSuccess bool
	}{
		{"post_success", 200, `{"qrcode":"post-qr","qrcode_img_content":"post-content"}`, 1, true},
		{"method_rejected", 405, `method not allowed`, 2, true},
		{"forbidden", 403, `forbidden`, 1, false},
		{"rate_limited", 429, `rate limited`, 1, false},
		{"server_error", 503, `unavailable`, 1, false},
		{"business_error", 200, `{"ret":123}`, 1, false},
		{"incomplete_qr", 200, `{"qrcode":"partial"}`, 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				attempt := calls.Add(1)
				if q.URL.Path != "/ilink/bot/get_bot_qrcode" || q.URL.RawQuery != "bot_type=3" || q.Header.Get("Authorization") != "" {
					t.Error("QR request leaked credentials or changed endpoint")
				}
				if attempt == 1 {
					var body struct {
						Tokens []string `json:"local_token_list"`
					}
					if q.Method != http.MethodPost || json.NewDecoder(q.Body).Decode(&body) != nil || len(body.Tokens) != 1 || body.Tokens[0] != "previous-private-token" {
						t.Error("initial QR request did not use documented POST format")
					}
					w.WriteHeader(test.status)
					io.WriteString(w, test.body)
					return
				}
				body, _ := io.ReadAll(q.Body)
				if attempt != 2 || q.Method != http.MethodGet || len(body) != 0 {
					t.Error("compatibility request must be one bodyless GET")
				}
				for _, header := range []string{"AuthorizationType", "X-WECHAT-UIN", "Content-Type"} {
					if q.Header.Get(header) != "" {
						t.Errorf("GET included POST metadata %s", header)
					}
				}
				if q.Header.Get("iLink-App-Id") != "bot" || q.Header.Get("iLink-App-ClientVersion") != "513" {
					t.Error("GET application metadata missing")
				}
				io.WriteString(w, `{"qrcode":"get-qr","qrcode_img_content":"get-content"}`)
			}))
			defer fake.Close()
			client := NewClient(Credentials{Endpoint: fake.URL, Token: "must-not-authorize"})
			qr, err := client.QR(context.Background(), "previous-private-token")
			if (err == nil) != test.wantSuccess || calls.Load() != test.wantCalls {
				t.Fatalf("unexpected QR result: calls=%d err=%v", calls.Load(), err)
			}
			if test.status == 405 && (qr.ID != "get-qr" || qr.Content != "get-content") {
				t.Fatal("GET response was not used")
			}
		})
	}
}

func TestQRCompatibilityFailureAndRedactedHTTPError(t *testing.T) {
	var calls atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		calls.Add(1)
		w.WriteHeader(405)
		io.WriteString(w, "secret-response-body")
	}))
	defer fake.Close()
	client := NewClient(Credentials{Endpoint: fake.URL})
	_, err := client.QR(context.Background(), "secret-token")
	var status *HTTPError
	if !errors.As(err, &status) || status.Method != "GET" || status.StatusCode != 405 || calls.Load() != 2 {
		t.Fatalf("unexpected compatibility failure: %v", err)
	}
	if !strings.Contains(err.Error(), "POST") || !strings.Contains(err.Error(), "GET") || !strings.Contains(err.Error(), "get_bot_qrcode") {
		t.Fatal("error omitted method or endpoint information")
	}
	_, err = client.LoginStatus(context.Background(), "secret-qr-session", "secret-verification-code")
	if !errors.As(err, &status) || status.Operation != "get_qrcode_status" || calls.Load() != 3 {
		t.Fatal("QR status request was retried or error context was lost")
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "?") {
		t.Fatal("HTTP error leaked query or response contents")
	}
	client.Token = "private-bot-token"
	_, err = client.SendItems(context.Background(), "owner", "context", nil)
	if !errors.As(err, &status) || status.Method != "POST" || calls.Load() != 4 {
		t.Fatal("message sending incorrectly retried using GET")
	}
}

func TestMediaCipherVectors(t *testing.T) {
	key, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	plain, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	cipher, err := encryptMedia(plain, key)
	if err != nil {
		t.Fatal(err)
	}
	// AES-128 known-answer vector; an extra block carries PKCS#7 padding.
	if hex.EncodeToString(cipher[:16]) != "69c4e0d86a7b0430d8cdb78070b4c55a" || len(cipher) != 32 {
		t.Fatal("AES known-answer mismatch")
	}
	for size := 0; size < 65; size++ {
		text := bytes.Repeat([]byte{byte(size)}, size)
		encrypted, err := encryptMedia(text, key)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decryptMedia(encrypted, key)
		if err != nil || !bytes.Equal(decoded, text) {
			t.Fatal("media round trip failed")
		}
	}
	if _, err := decryptMedia(cipher[:17], key); err == nil {
		t.Fatal("truncated ciphertext accepted")
	}
	for _, encoded := range []string{base64.StdEncoding.EncodeToString(key), base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(key)))} {
		got, err := mediaKey(encoded)
		if err != nil || !bytes.Equal(got, key) {
			t.Fatal("supported key encoding rejected")
		}
	}
	if _, err := mediaKey("invalid"); err == nil {
		t.Fatal("invalid key accepted")
	}
}

func TestAttachmentUploadAndSafeDownload(t *testing.T) {
	plaintext := []byte("文件内容：会议纪要")
	var encrypted []byte
	var uploadKey []byte
	var endpoint string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		switch q.URL.Path {
		case "/ilink/bot/getuploadurl":
			var body struct {
				MediaType int    `json:"media_type"`
				Key       string `json:"aeskey"`
				RawSize   int    `json:"rawsize"`
				Size      int    `json:"filesize"`
				To        string `json:"to_user_id"`
			}
			if err := json.NewDecoder(q.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.MediaType != 3 || body.RawSize != len(plaintext) || body.Size%16 != 0 || body.To != "owner" {
				t.Errorf("invalid upload metadata: %+v", body)
			}
			uploadKey, _ = hex.DecodeString(body.Key)
			json.NewEncoder(w).Encode(map[string]string{"upload_full_url": endpoint + "/upload"})
		case "/upload":
			if q.Header.Get("Authorization") != "" {
				t.Error("bot token leaked to CDN")
			}
			encrypted, _ = io.ReadAll(q.Body)
			decoded, err := decryptMedia(encrypted, uploadKey)
			if err != nil || !bytes.Equal(decoded, plaintext) {
				t.Error("uploaded file was not encrypted correctly")
			}
			w.Header().Set("x-encrypted-param", "download-ticket")
			w.WriteHeader(200)
		case "/download":
			if q.Header.Get("Authorization") != "" {
				t.Error("download leaked bot token")
			}
			w.Write(encrypted)
		default:
			t.Errorf("unexpected path %s", q.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer fake.Close()
	endpoint = fake.URL
	path := filepath.Join(t.TempDir(), "minutes.txt")
	if err := os.WriteFile(path, plaintext, 0600); err != nil {
		t.Fatal(err)
	}
	client := NewClient(Credentials{Endpoint: fake.URL, Token: "token"})
	item, err := client.Upload(context.Background(), "owner", "file", path)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(item)
	var outgoing Item
	if err = json.Unmarshal(encoded, &outgoing); err != nil {
		t.Fatal(err)
	}
	if outgoing.Type != 4 || outgoing.File.Media.Query != "download-ticket" {
		t.Fatal("wrong media message")
	}
	outgoing.File.Media.URL = endpoint + "/download"
	outgoing.File.Name = "../../sensitive.txt"
	dir := t.TempDir()
	saved := client.SaveAttachment(context.Background(), dir, "event", 0, outgoing)
	if saved.Error != "" {
		t.Fatal(saved.Error)
	}
	if filepath.Dir(saved.Path) != dir {
		t.Fatal("filename escaped media directory")
	}
	downloaded, err := os.ReadFile(saved.Path)
	if err != nil || !bytes.Equal(downloaded, plaintext) {
		t.Fatal("downloaded file mismatch")
	}
	if err = validateCDN("https://attacker.example/download"); err == nil {
		t.Fatal("untrusted CDN URL accepted")
	}
}

func TestNoAutomaticRetryOfAmbiguousSend(t *testing.T) {
	var attempts atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { attempts.Add(1); w.WriteHeader(503) }))
	defer fake.Close()
	relay := testRelay(t, fake.URL)
	if err := relay.Journal.Update(func(s *Snapshot) { s.ReplyTokens["owner-one"] = "context" }); err != nil {
		t.Fatal(err)
	}
	result, err := relay.Send(context.Background(), SendRequest{Text: "message"})
	if err == nil || attempts.Load() != 1 || len(result.IDs) != 0 {
		t.Fatal("ambiguous send was retried")
	}
}
