package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const defaultUpstream = "https://api.deepseek.com"

func main() {
	upstreamRaw := strings.TrimSpace(os.Getenv("UPSTREAM"))
	if upstreamRaw == "" {
		upstreamRaw = defaultUpstream
	}
	upstream, err := url.Parse(upstreamRaw)
	if err != nil || upstream.Scheme == "" || upstream.Host == "" {
		log.Fatalf("invalid UPSTREAM: %q", upstreamRaw)
	}

	listen := strings.TrimSpace(os.Getenv("LISTEN"))
	if listen == "" {
		listen = ":8080"
	}

	modelMap := buildModelMap()
	dsChatOpts := loadDeepSeekChatOptions()

	rp := httputil.NewSingleHostReverseProxy(upstream)
	rp.FlushInterval = -1
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, e error) {
		log.Printf("proxy error: %v", e)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream error","type":"proxy_error"}}`))
	}

	orig := rp.Director
	rp.Director = func(req *http.Request) {
		orig(req)
		req.Host = upstream.Host
		req.URL.Host = upstream.Host
		req.URL.Scheme = upstream.Scheme
		req.Header.Del("Accept-Encoding")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		withCORS(w, r, func() {
			path := r.URL.Path
			if r.Method == http.MethodPost && path == "/v1/chat/completions" {
				if err := rewriteChatCompletionBody(r, modelMap, dsChatOpts); err != nil {
					log.Printf("rewrite chat body: %v", err)
					jsonErr(w, http.StatusBadRequest, "invalid request body")
					return
				}
			}
			isV1 := path == "/v1" || strings.HasPrefix(path, "/v1/")
			if !isV1 {
				if path == "/" {
					w.Header().Set("Content-Type", "text/plain; charset=utf-8")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte("cursor-deepseek-bridge: set OpenAI Base URL to http://HOST:PORT/v1\n"))
					return
				}
				jsonErr(w, http.StatusNotFound, "not found")
				return
			}
			rp.ServeHTTP(w, r)
		})
	})

	s := &http.Server{
		Addr:              listen,
		Handler:           logRequest(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}

	if dsChatOpts.wantThinking {
		log.Printf("DeepSeek thinking=enabled: multi-turn needs reasoning_content in history; Cursor often cannot — set DS_THINKING=disabled to avoid.")
	}
	log.Printf("listening %s -> %s", listen, upstream.String())
	if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// buildModelMap 將 Cursor / OpenAI 客戶端常用 model 名稱對應到 DeepSeek。
// MAPPED_MODEL 預設 deepseek-v4-pro；若要改回 flash 可設 MAPPED_MODEL=deepseek-v4-flash。
func buildModelMap() map[string]string {
	target := strings.TrimSpace(os.Getenv("MAPPED_MODEL"))
	if target == "" {
		target = "deepseek-v4-pro"
	}
	m := map[string]string{
		"gpt-4o":            target,
		"gpt-4o-mini":       target,
		"gpt-4":             target,
		"gpt-4-turbo":       target,
		"gpt-3.5-turbo":     target,
		"chatgpt-4o-latest": target,
	}
	if raw := strings.TrimSpace(os.Getenv("MODEL_MAP")); raw != "" {
		parts := strings.Split(raw, ",")
		for _, p := range parts {
			kv := strings.SplitN(strings.TrimSpace(p), "=", 2)
			if len(kv) != 2 {
				continue
			}
			from, to := strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])
			if from != "" && to != "" {
				m[from] = to
			}
		}
	}
	return m
}

type deepSeekChatOptions struct {
	wantThinking    bool
	thinkingType    string // "enabled" when wantThinking
	reasoningEffort string
}

// DeepSeek V4 預設 thinking=enabled（見 https://api-docs.deepseek.com/guides/thinking_mode ），
// Cursor 多輪不會帶回 reasoning_content，故預設強制 thinking=disabled。
// 開啟推理：DS_V4_PRO_DEFAULTS=1 或 DS_THINKING=enabled；關閉：DS_THINKING=disabled。
func loadDeepSeekChatOptions() deepSeekChatOptions {
	o := deepSeekChatOptions{}
	if envTruthy(strings.TrimSpace(os.Getenv("DS_V4_PRO_DEFAULTS"))) {
		o.wantThinking = true
	}

	dsThink := strings.TrimSpace(os.Getenv("DS_THINKING"))
	switch {
	case strings.EqualFold(dsThink, "disabled"):
		o.wantThinking = false
	case dsThink != "" && (strings.EqualFold(dsThink, "enabled") || envTruthy(dsThink)):
		o.wantThinking = true
	}

	if v := strings.TrimSpace(os.Getenv("DS_REASONING_EFFORT")); v != "" {
		o.reasoningEffort = v
	}
	if o.wantThinking {
		o.thinkingType = "enabled"
		if o.reasoningEffort == "" {
			o.reasoningEffort = "high"
		}
	}
	return o
}

func envTruthy(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "1" || s == "true" || s == "yes" || s == "on"
}

func rewriteChatCompletionBody(r *http.Request, modelMap map[string]string, opts deepSeekChatOptions) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	_ = r.Body.Close()
	if len(bytes.TrimSpace(body)) == 0 {
		r.Body = io.NopCloser(bytes.NewReader(body))
		return nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return err
	}

	changed := false
	if rawModel, ok := payload["model"]; ok {
		var modelStr string
		if err := json.Unmarshal(rawModel, &modelStr); err == nil {
			if repl, ok := modelMap[modelStr]; ok {
				newRaw, err := json.Marshal(repl)
				if err != nil {
					return err
				}
				payload["model"] = newRaw
				changed = true
			}
		}
	}

	if !opts.wantThinking {
		dis, err := json.Marshal(map[string]string{"type": "disabled"})
		if err != nil {
			return err
		}
		payload["thinking"] = dis
		delete(payload, "reasoning_effort")
		changed = true
	} else {
		if opts.thinkingType != "" {
			if _, exists := payload["thinking"]; !exists {
				thinkingObj, err := json.Marshal(map[string]string{"type": opts.thinkingType})
				if err != nil {
					return err
				}
				payload["thinking"] = thinkingObj
				changed = true
			}
		}
		if opts.reasoningEffort != "" {
			if _, exists := payload["reasoning_effort"]; !exists {
				raw, err := json.Marshal(opts.reasoningEffort)
				if err != nil {
					return err
				}
				payload["reasoning_effort"] = raw
				changed = true
			}
		}
	}

	if !changed {
		r.Body = io.NopCloser(bytes.NewReader(body))
		return nil
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(out))
	r.ContentLength = int64(len(out))
	r.Header.Set("Content-Length", strconv.Itoa(len(out)))
	r.Header.Del("Transfer-Encoding")
	return nil
}

func jsonErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": msg, "type": "invalid_request_error"},
	})
}

func withCORS(w http.ResponseWriter, r *http.Request, fn func()) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	fn()
}

func logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
