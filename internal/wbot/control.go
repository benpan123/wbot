package wbot

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

func (r *Relay) Handler() http.Handler {
	routes := http.NewServeMux()
	routes.HandleFunc("GET /v1/status", func(w http.ResponseWriter, q *http.Request) { respond(w, 200, r.Status()) })
	routes.HandleFunc("POST /v1/send", func(w http.ResponseWriter, q *http.Request) {
		if !strings.HasPrefix(q.Header.Get("Content-Type"), "application/json") {
			respond(w, 415, map[string]string{"error": "请求需要使用 application/json"})
			return
		}
		q.Body = http.MaxBytesReader(w, q.Body, 1<<20)
		decoder := json.NewDecoder(q.Body)
		decoder.DisallowUnknownFields()
		var request SendRequest
		if err := decoder.Decode(&request); err != nil {
			respond(w, 400, map[string]string{"error": "发送参数格式无效"})
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			respond(w, 400, map[string]string{"error": "请求只能包含一个 JSON 对象"})
			return
		}
		result, err := r.Send(q.Context(), request)
		if err != nil {
			respond(w, 502, map[string]any{"error": err.Error(), "sent_message_ids": result.IDs})
			return
		}
		respond(w, 200, result)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if subtle.ConstantTimeCompare([]byte(q.Header.Get("Authorization")), []byte("Bearer "+r.Config.LocalToken)) != 1 {
			respond(w, 401, map[string]string{"error": "本地接口身份验证失败"})
			return
		}
		routes.ServeHTTP(w, q)
	})
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (r *Relay) Serve(ctx context.Context, listener net.Listener) error {
	server := &http.Server{Handler: r.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	ended := make(chan error, 1)
	go func() { ended <- server.Serve(listener) }()
	select {
	case err := <-ended:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
		err := <-ended
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
