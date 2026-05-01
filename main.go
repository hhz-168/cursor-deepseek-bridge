package main

import (
	"bytes"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const defaultUpstream = "https://api.deepseek.com"

// 請求與回應 body 大小限制
// chat completions 含 base64 圖片時體積大；預設 32 MiB，可用 DS_MAX_REQUEST_BODY 覆寫（如 64m、8388608）
const defaultMaxChatRequestBody = 32 << 20 // 32 MiB
const maxResponseBodySize = 10 << 20         // 10 MB

var maxChatRequestBodyBytes int64 = defaultMaxChatRequestBody

// loadMaxChatRequestBodyBytes 讀取環境變數 DS_MAX_REQUEST_BODY。
// 支援：純數字（bytes）、"32m"/"32mb"/"32mib"（MiB）、"1g"/"1gb"（GiB）、"512k"/"512kb"（KiB）。
// 無效值則用預設；最小 1 MiB，最大 256 MiB。
func loadMaxChatRequestBodyBytes() int64 {
	def := int64(defaultMaxChatRequestBody)
	v := strings.TrimSpace(os.Getenv("DS_MAX_REQUEST_BODY"))
	if v == "" {
		return def
	}
	s := strings.ToLower(v)
	var mult int64 = 1
	cut := v
	switch {
	case strings.HasSuffix(s, "gib"):
		mult = 1 << 30
		cut = strings.TrimSpace(v[:len(v)-3])
	case strings.HasSuffix(s, "gb"):
		mult = 1 << 30
		cut = strings.TrimSpace(v[:len(v)-2])
	case strings.HasSuffix(s, "g") && len(v) > 1:
		mult = 1 << 30
		cut = strings.TrimSpace(v[:len(v)-1])
	case strings.HasSuffix(s, "mib"):
		mult = 1 << 20
		cut = strings.TrimSpace(v[:len(v)-3])
	case strings.HasSuffix(s, "mb"):
		mult = 1 << 20
		cut = strings.TrimSpace(v[:len(v)-2])
	case strings.HasSuffix(s, "m") && len(v) > 1:
		mult = 1 << 20
		cut = strings.TrimSpace(v[:len(v)-1])
	case strings.HasSuffix(s, "kib"):
		mult = 1 << 10
		cut = strings.TrimSpace(v[:len(v)-3])
	case strings.HasSuffix(s, "kb"):
		mult = 1 << 10
		cut = strings.TrimSpace(v[:len(v)-2])
	case strings.HasSuffix(s, "k") && len(v) > 1:
		mult = 1 << 10
		cut = strings.TrimSpace(v[:len(v)-1])
	default:
		cut = v
	}
	n, err := strconv.ParseInt(strings.TrimSpace(cut), 10, 64)
	if err != nil || n <= 0 {
		log.Printf("[config] invalid DS_MAX_REQUEST_BODY %q, using default %d", v, def)
		return def
	}
	out := n * mult
	minB := int64(1 << 20)
	if out < minB {
		out = minB
	}
	maxCap := int64(256 << 20)
	if out > maxCap {
		log.Printf("[config] DS_MAX_REQUEST_BODY capped at 256 MiB")
		out = maxCap
	}
	return out
}

// ctxKeyConv 是 context 中存放 conversation fingerprint 的 key
type ctxKeyConv struct{}

// ctxKeyConvFromCookie 标记本次 convKey 是否来自 cookie（用于调试）
type ctxKeyConvFromCookie struct{}

// makeConvCookie 生成 conv_id cookie 的 Set-Cookie 字符串。
// convKey 是 sha256 hex（64 字符），作為穩定對話唯一標識。
// 瀏覽器 / HTTP 客戶端會自動帶回此 cookie，確保同一對話的所有請求使用相同 key。
func makeConvCookie(convID string) string {
	const maxAge = 86400 * 7 // 7 天，足以覆蓋一個對話的生命週期
	return fmt.Sprintf("conv_id=%s; Path=/; HttpOnly; SameSite=Lax; Max-Age=%d", convID, maxAge)
}

// thinkingCache 保存 assistant message content 的 SHA-256 → reasoning_content，用於多輪對話時補回 Cursor 缺失的 reasoning_content。
type thinkingCache struct {
	mu   sync.RWMutex
	m    map[string]cacheEntry
	ttl  time.Duration
	stop chan struct{}
	once sync.Once
}

type cacheEntry struct {
	reasoning string
	expireAt  time.Time
}

func newThinkingCache(ttl time.Duration) *thinkingCache {
	c := &thinkingCache{
		m:    make(map[string]cacheEntry),
		ttl:  ttl,
		stop: make(chan struct{}),
	}
	go c.cleaner()
	return c
}

func (c *thinkingCache) set(content, reasoning string) {
	if content == "" || reasoning == "" {
		return
	}
	content = normalizeTextForCache(content)
	key := hashContent(content)
	c.mu.Lock()
	c.m[key] = cacheEntry{reasoning: reasoning, expireAt: time.Now().Add(c.ttl)}
	c.mu.Unlock()
}

func (c *thinkingCache) get(content string) (string, bool) {
	if content == "" {
		return "", false
	}
	content = normalizeTextForCache(content)
	key := hashContent(content)
	c.mu.RLock()
	e, ok := c.m[key]
	c.mu.RUnlock()
	if !ok || time.Now().After(e.expireAt) {
		return "", false
	}
	return e.reasoning, true
}

func (c *thinkingCache) cleaner() {
	tick := time.NewTicker(1 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			now := time.Now()
			c.mu.Lock()
			for k, e := range c.m {
				if now.After(e.expireAt) {
					delete(c.m, k)
				}
			}
			c.mu.Unlock()
		case <-c.stop:
			return
		}
	}
}

func (c *thinkingCache) close() {
	c.once.Do(func() {
		close(c.stop)
	})
}

const defaultReasonOrderCap = 256

// reasoningOrderQueue 記錄依時間順序的每輪 assistant 推論結果，用於內容哈希失敗時依序對齊注入。
type reasoningOrderQueue struct {
	mu    sync.RWMutex
	items []assistantReasonPair
	cap   int
}

type assistantReasonPair struct {
	plain     string
	reasoning string
}

func newReasoningOrderQueue(max int) *reasoningOrderQueue {
	if max <= 0 {
		max = defaultReasonOrderCap
	}
	return &reasoningOrderQueue{cap: max}
}

func (q *reasoningOrderQueue) push(plain, reasoning string) {
	if reasoning == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	plain = normalizeTextForCache(plain)
	q.items = append(q.items, assistantReasonPair{plain: plain, reasoning: reasoning})
	for len(q.items) > q.cap {
		q.items = q.items[1:]
	}
}

// lastK 回傳隊列末尾最多 k 條記錄，時間由舊到新（與 messages 中 assistant 由舊到新的順序對齊）。
func (q *reasoningOrderQueue) lastK(k int) []assistantReasonPair {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if k <= 0 || len(q.items) == 0 {
		return nil
	}
	if k > len(q.items) {
		k = len(q.items)
	}
	start := len(q.items) - k
	out := make([]assistantReasonPair, k)
	copy(out, q.items[start:])
	return out
}

// reasoningOrderQueues 管理按 conversation fingerprint 隔離的多個 order queue，
// 避免不同對話間的 reasoning 互相污染（Fix #3）。
// 每條子隊列有 TTL，超時無存取自動清理，避免記憶體洩漏。
type reasoningOrderQueues struct {
	mu         sync.RWMutex
	queues     map[string]*timedQueue
	defaultCap int
	ttl        time.Duration
	stop       chan struct{}
	once       sync.Once
}

type timedQueue struct {
	*reasoningOrderQueue
	lastAccess time.Time
}

func newReasoningOrderQueues(ttl time.Duration) *reasoningOrderQueues {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	r := &reasoningOrderQueues{
		queues:     make(map[string]*timedQueue),
		defaultCap: defaultReasonOrderCap,
		ttl:        ttl,
		stop:       make(chan struct{}),
	}
	go r.cleaner()
	return r
}

func (r *reasoningOrderQueues) push(key, plain, reasoning string) {
	if reasoning == "" || key == "" {
		return
	}
	now := time.Now()
	r.mu.RLock()
	tq, ok := r.queues[key]
	r.mu.RUnlock()
	if !ok {
		r.mu.Lock()
		tq, ok = r.queues[key]
		if !ok {
			tq = &timedQueue{
				reasoningOrderQueue: newReasoningOrderQueue(r.defaultCap),
				lastAccess:          now,
			}
			r.queues[key] = tq
		}
		r.mu.Unlock()
	}
	tq.lastAccess = now
	tq.push(plain, reasoning)
}

func (r *reasoningOrderQueues) lastK(key string, k int) []assistantReasonPair {
	if key == "" {
		return nil
	}
	r.mu.RLock()
	tq, ok := r.queues[key]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	tq.lastAccess = time.Now()
	return tq.lastK(k)
}

func (r *reasoningOrderQueues) cleaner() {
	tick := time.NewTicker(r.ttl / 2)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			now := time.Now()
			r.mu.Lock()
			for k, tq := range r.queues {
				if now.Sub(tq.lastAccess) > r.ttl {
					delete(r.queues, k)
				}
			}
			r.mu.Unlock()
		case <-r.stop:
			return
		}
	}
}

func (r *reasoningOrderQueues) close() {
	r.once.Do(func() {
		close(r.stop)
	})
}

// computeConversationKey 從 messages 計算 conversation fingerprint，
// 用於 per-conversation order queue 的鍵值。穩定在同一對話的不同輪次中保持一致。
// 策略：hash(前 N 條非 system 的 user message) 作為對話指紋。
// 使用多條 user message 而非僅第一條，確保歷史截斷時仍有較高概率匹配到相同 fingerprifnt。
func computeConversationKey(msgs []json.RawMessage) string {
	const maxUserMsgs = 3
	h := sha256.New()
	userCount := 0
	for _, msg := range msgs {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(msg, &m) != nil {
			// 無法解析的消息直接作為 key 的一部分
			h.Write(bytes.TrimSpace(msg))
			h.Write([]byte{0})
			continue
		}
		if m.Role == "system" {
			continue // 跳過 system message，只關注 user message
		}
		if m.Role == "user" {
			content := normalizeTextForCache(flattenContentField(m.Content))
			h.Write([]byte(m.Role))
			h.Write([]byte{0})
			h.Write([]byte(content))
			h.Write([]byte{0})
			userCount++
			if userCount >= maxUserMsgs {
				break
			}
		}
	}
	if userCount == 0 {
		// 如果沒有任何 user message，回退到 hash 全部非 system 消息
		for _, msg := range msgs {
			h.Write(bytes.TrimSpace(msg))
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// recordAssistantReasoning 同時更新內容哈希緩存與 per-conversation 順序隊列；plain 可為空（僅 tool_calls 無正文時）。
func recordAssistantReasoning(cache *thinkingCache, orderQueues *reasoningOrderQueues, convKey, plain, reasoning string) {
	if reasoning == "" {
		return
	}
	if plain != "" {
		cache.set(plain, reasoning)
	}
	if convKey != "" {
		orderQueues.push(convKey, plain, reasoning)
	}
}

func hashContent(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// normalizeTextForCache 統一空白與換行，降低 Cursor 與流式拼字串的細微差異導致緩存未命中。
func normalizeTextForCache(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.TrimSpace(s)
}

// flattenContentField 將 OpenAI / Cursor 中 message.content 轉成單一純文字（字串、null、或 content part 陣列）。
func flattenContentField(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var arr []struct {
		Type string          `json:"type"`
		Text json.RawMessage `json:"text"`
	}
	if json.Unmarshal(raw, &arr) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range arr {
		switch p.Type {
		case "text":
			var t string
			if json.Unmarshal(p.Text, &t) == nil {
				b.WriteString(t)
				continue
			}
			var obj struct {
				Value string `json:"value"`
			}
			if json.Unmarshal(p.Text, &obj) == nil {
				b.WriteString(obj.Value)
			}
		}
	}
	return b.String()
}

// sanitizeMessageContent 處理單條消息中的多媒體內容：
//   - 對 image_url 類型的 content part 執行 OCR（如 Python worker 可用）
//   - OCR 結果以 text part 插入，包含辨識文字與位置資訊
//   - 若 OCR 不可用或失敗，插入說明文字替代原始圖片
//   - 原始 image_url part 被移除
// 返回處理後的 message JSON 以及是否發生了變更。
func sanitizeMessageContent(raw json.RawMessage) (json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw, false
	}
	cbuf, ok := m["content"]
	if !ok || len(cbuf) == 0 {
		return raw, false
	}

	// 先嘗試純字串——無需處理
	var s string
	if json.Unmarshal(cbuf, &s) == nil {
		return raw, false
	}

	// 嘗試解析為 content parts 陣列
	var arr []json.RawMessage
	if json.Unmarshal(cbuf, &arr) != nil {
		return raw, false
	}

	type contentPart struct {
		Type     string          `json:"type"`
		Text     json.RawMessage `json:"text"`
		ImageURL json.RawMessage `json:"image_url,omitempty"`
	}

	var hasImage bool
	var resultParts []json.RawMessage
	for _, part := range arr {
		var cp contentPart
		if json.Unmarshal(part, &cp) != nil {
			resultParts = append(resultParts, part)
			continue
		}
		if cp.Type == "text" {
			resultParts = append(resultParts, part)
		} else if cp.Type == "image_url" {
			hasImage = true
			ocrText := ""

			if globalOcrProcess != nil {
				// 解析 image_url 中的 url
				var iu struct {
					URL string `json:"url"`
				}
				if err := json.Unmarshal(cp.ImageURL, &iu); err == nil && iu.URL != "" {
					// 取得圖片資料
					imgData, err := resolveImageData(iu.URL)
					if err != nil {
						log.Printf("[ocr] resolve image failed: %v (url=%s...)", err, truncateStr(iu.URL, 60))
					} else {
						// 執行 OCR
						ocrText, err = globalOcrProcess.doOcr(imgData)
						if err != nil {
							log.Printf("[ocr] OCR failed: %v", err)
						} else {
							log.Printf("[ocr] image processed successfully, text length=%d", len(ocrText))
						}
					}
				}
			} else {
				log.Printf("[ocr] OCR engine not available")
			}

			if ocrText == "" {
				ocrText = "[Image attached - OCR was unable to process this image]"
			}

			// 將 OCR 結果作為 text part 插入
			ocrTextRaw, _ := json.Marshal(ocrText)
			ocrPart := map[string]json.RawMessage{
				"type": json.RawMessage(`"text"`),
				"text": ocrTextRaw,
			}
			ocrPartRaw, _ := json.Marshal(ocrPart)
			resultParts = append(resultParts, ocrPartRaw)
		}
	}

	if !hasImage {
		return raw, false
	}

	// 重組 content
	if len(resultParts) == 0 {
		m["content"], _ = json.Marshal("")
	} else if len(resultParts) == 1 {
		// 只有一個 text part，簡化為純字串
		var cp contentPart
		if json.Unmarshal(resultParts[0], &cp) == nil && cp.Type == "text" {
			var txt string
			if json.Unmarshal(cp.Text, &txt) == nil {
				m["content"], _ = json.Marshal(txt)
			} else {
				var obj struct {
					Value string `json:"value"`
				}
				if json.Unmarshal(cp.Text, &obj) == nil {
					m["content"], _ = json.Marshal(obj.Value)
				} else {
					m["content"], _ = json.Marshal(resultParts)
				}
			}
		} else {
			m["content"], _ = json.Marshal(resultParts)
		}
	} else {
		m["content"], _ = json.Marshal(resultParts)
	}

	result, err := json.Marshal(m)
	if err != nil {
		return raw, false
	}
	return result, true
}

// decodeImageFromDataURI 解析 data:image/... 格式的 base64 URI，返回圖片解碼後的 bytes。
func decodeImageFromDataURI(uri string) ([]byte, error) {
	// data:[<mediatype>][;base64],<data>
	comma := strings.Index(uri, ",")
	if comma < 0 {
		return nil, fmt.Errorf("invalid data URI: no comma")
	}
	encoded := uri[comma+1:]
	// 解碼 base64（支援 standard 與 URL-safe）
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(encoded)
	}
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	return decoded, nil
}

// resolveImageData 從 data URI 或 HTTP URL 解析圖片 bytes。
func resolveImageData(uri string) ([]byte, error) {
	if strings.HasPrefix(uri, "data:") {
		return decodeImageFromDataURI(uri)
	}
	// HTTP(S) URL 下載（僅支援 http/https）
	if strings.HasPrefix(uri, "http://") || strings.HasPrefix(uri, "https://") {
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Get(uri)
		if err != nil {
			return nil, fmt.Errorf("download image: %w", err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20)) // 20MB 限制
		if err != nil {
			return nil, fmt.Errorf("read image: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("download image: HTTP %d", resp.StatusCode)
		}
		// 檢查是否為有效圖片（讀取 header）
		_, _, err = image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("invalid image: %w", err)
		}
		return data, nil
	}
	return nil, fmt.Errorf("unsupported image URI scheme: only data: and http(s): are supported")
}

// truncateStr 截斷字串到指定長度（用於 log 輸出）
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// assistantPlainTextFromMessageRaw 從完整 message 對象取出 role 與用於緩存的純文本 content。
func assistantPlainTextFromMessageRaw(rawMsg json.RawMessage) (role, plain string) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(rawMsg, &m); err != nil {
		log.Printf("assistantPlainTextFromMessageRaw: json unmarshal error: %v", err)
		return "", ""
	}
	rbuf, ok := m["role"]
	if !ok {
		return "", ""
	}
	if err := json.Unmarshal(rbuf, &role); err != nil {
		log.Printf("assistantPlainTextFromMessageRaw: role unmarshal error: %v", err)
		return "", ""
	}
	if role != "assistant" {
		return role, ""
	}
	if cbuf, ok := m["content"]; ok {
		plain = normalizeTextForCache(flattenContentField(cbuf))
	}
	return role, plain
}

// assistantMessageNeedsReasoningInject 為 assistant 且尚未帶上非空 reasoning_content 時需代理補回。
func assistantMessageNeedsReasoningInject(rawMsg json.RawMessage) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(rawMsg, &m); err != nil {
		log.Printf("assistantMessageNeedsReasoningInject: json unmarshal error: %v", err)
		return false
	}
	rbuf, ok := m["role"]
	if !ok {
		return false
	}
	var role string
	if err := json.Unmarshal(rbuf, &role); err != nil {
		return false
	}
	if role != "assistant" {
		return false
	}
	if rc, ok := m["reasoning_content"]; ok {
		// reasoning_content: null 或空字串都需要補回
		var s string
		if json.Unmarshal(rc, &s) == nil && strings.TrimSpace(s) != "" {
			return false
		}
	}
	return true
}

type chatMessage struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type chatMessageFlexible struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
}

type chatCompletionChoice struct {
	Index        int                  `json:"index"`
	Message      *chatMessageFlexible `json:"message,omitempty"`
	Delta        *chatMessage         `json:"delta,omitempty"`
	FinishReason string               `json:"finish_reason,omitempty"`
}

type chatCompletionResponse struct {
	ID      string                 `json:"id"`
	Choices []chatCompletionChoice `json:"choices"`
}

// cacheReasoningFromSSE 解析 OpenAI 相容 SSE，累積各 chunk 的 delta.reasoning_content 與 delta.content。
// 支援多個 choices（n > 1），每個 choice 獨立累積 reasoning_content。
func cacheReasoningFromSSE(body []byte, cache *thinkingCache, orderQueues *reasoningOrderQueues, convKey string) {
	type choiceAccum struct {
		reasoning strings.Builder
		content   strings.Builder
	}
	accum := make(map[int]*choiceAccum)

	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == ':' {
			continue // 空行或 SSE comment（如 : keep-alive）
		}
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data: "):])
		if bytes.Equal(payload, []byte("[DONE]")) {
			break
		}
		var chunk struct {
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(payload, &chunk) != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			idx := ch.Index
			if _, ok := accum[idx]; !ok {
				accum[idx] = &choiceAccum{}
			}
			accum[idx].reasoning.WriteString(ch.Delta.ReasoningContent)
			accum[idx].content.WriteString(ch.Delta.Content)
		}
	}

	// 按 index 順序輸出（0, 1, 2, ...）
	for idx := 0; ; idx++ {
		ac, ok := accum[idx]
		if !ok {
			break
		}
		rs := ac.reasoning.String()
		cs := normalizeTextForCache(ac.content.String())
		if rs == "" {
			continue
		}
		cache.set(cs, rs)
		if convKey != "" {
			orderQueues.push(convKey, cs, rs)
		}
	}
}

// reasoningCacheTransport 對 SSE 使用 TeeReader：客戶端仍即時收到流，關閉連接時再解析並緩存 reasoning。
type reasoningCacheTransport struct {
	rt          http.RoundTripper
	cache       *thinkingCache
	orderQueues *reasoningOrderQueues
}

func (t *reasoningCacheTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt := t.rt
	if rt == nil {
		rt = http.DefaultTransport
	}
	resp, err := rt.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	// 在 SSE 響應中也設置 conv_id cookie
	if convKey, ok := req.Context().Value(ctxKeyConv{}).(string); ok && convKey != "" {
		cookieStr := makeConvCookie(convKey)
		resp.Header.Add("Set-Cookie", cookieStr)
		log.Printf("[sse] Set-Cookie conv_id=%s", convKey[:min(len(convKey), 8)])
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		return resp, err
	}
	// 從請求 context 中取出 conversation key
	convKey, _ := req.Context().Value(ctxKeyConv{}).(string)
	buf := &bytes.Buffer{}
	orig := resp.Body
	resp.Body = &sseTeeReadCloser{
		r:       io.TeeReader(orig, buf),
		orig:    orig,
		buf:     buf,
		cache:   t.cache,
		order:   t.orderQueues,
		convKey: convKey,
	}
	return resp, nil
}

type sseTeeReadCloser struct {
	r       io.Reader
	orig    io.ReadCloser
	buf     *bytes.Buffer
	cache   *thinkingCache
	order   *reasoningOrderQueues
	convKey string
	once    sync.Once
}

func (s *sseTeeReadCloser) Read(p []byte) (int, error) {
	return s.r.Read(p)
}

func (s *sseTeeReadCloser) Close() error {
	err := s.orig.Close()
	s.once.Do(func() {
		cacheReasoningFromSSE(s.buf.Bytes(), s.cache, s.order, s.convKey)
	})
	return err
}

// OCR 子进程管理

// ocrWorkerResponse 是 Python ocr_worker.py 返回的 JSON 格式
type ocrWorkerResponse struct {
	Success bool            `json:"success"`
	Text    string          `json:"text"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error,omitempty"`
}

// ocrProcess 管理一個持久化的 Python OCR 子行程，支援自動重啟
type ocrProcess struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	stderr bytes.Buffer
	dead   bool // true 表示行程已退出
}

// startOcrProcess 啟動 ocr_worker.py 子行程。
// PYTHON 環境變數可指定 python 路徑（預設 "python3"）。
func startOcrProcess() (*ocrProcess, error) {
	workerScript := "ocr_worker.py"
	// 嘗試尋找 worker script 的絕對路徑
	if _, err := os.Stat(workerScript); os.IsNotExist(err) {
		if _, err := os.Stat(filepath.Join("..", workerScript)); err == nil {
			workerScript = filepath.Join("..", workerScript)
		}
	}

	pythonCmd := strings.TrimSpace(os.Getenv("PYTHON"))
	if pythonCmd == "" {
		// Windows 上優先嘗試 python，linux/macOS 上優先嘗試 python3
		pythonCmd = "python3"
	}

	// 先檢查 python 是否可用
	if _, err := exec.LookPath(pythonCmd); err != nil {
		// 嘗試其他名稱
		for _, alt := range []string{"python", "python3", "python3.exe", "python.exe"} {
			if _, err := exec.LookPath(alt); err == nil {
				pythonCmd = alt
				break
			}
		}
	}

	cmd := exec.Command(pythonCmd, "-u", workerScript)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr // 讓 python 的 stderr 直接輸出到 bridge 的 stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start python process: %w", err)
	}

	p := &ocrProcess{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewScanner(stdoutPipe),
	}

	// 啟動後台 goroutine 監控進程退出
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.dead = true
		p.mu.Unlock()
		if err != nil {
			log.Printf("[ocr] Python OCR worker exited (pid=%d): %v", cmd.Process.Pid, err)
		} else {
			log.Printf("[ocr] Python OCR worker exited normally (pid=%d)", cmd.Process.Pid)
		}
	}()

	log.Printf("[ocr] Python OCR worker started (pid=%d)", cmd.Process.Pid)
	return p, nil
}

// restartOcrProcess 重啟已掛掉的 OCR 子行程
func restartOcrProcess() error {
	globalOcrProcess.mu.Lock()
	defer globalOcrProcess.mu.Unlock()

	// 檢查是否真的需要重啟
	if globalOcrProcess != nil && !globalOcrProcess.dead {
		return nil // 還活著，不需要重啟
	}

	log.Printf("[ocr] restarting Python OCR worker...")
	p, err := startOcrProcess()
	if err != nil {
		return fmt.Errorf("restart failed: %w", err)
	}
	globalOcrProcess = p
	return nil
}

// doOcr 對圖片 bytes 執行 OCR，返回格式化文字。
// 線程安全（內部有 mutex 保護）。如果子行程掛掉，自動重啟重試一次。
func (p *ocrProcess) doOcr(imgData []byte) (string, error) {
	if p == nil {
		return "", fmt.Errorf("OCR process not available")
	}

	for attempt := 0; attempt < 2; attempt++ {
		text, err := p.doOcrOnce(imgData)
		if err == nil {
			return text, nil
		}
		// 只在第一次失敗時嘗試重啟
		if attempt == 0 && strings.Contains(err.Error(), "pipe") || strings.Contains(err.Error(), "closed") {
			log.Printf("[ocr] worker seems dead, restarting...")
			if restartErr := restartOcrProcess(); restartErr != nil {
				log.Printf("[ocr] restart failed: %v", restartErr)
				return "", fmt.Errorf("OCR failed and could not restart: %w", err)
			}
			continue
		}
		return "", err
	}
	return "", fmt.Errorf("OCR failed after retry")
}

// doOcrOnce 單次 OCR 調用
func (p *ocrProcess) doOcrOnce(imgData []byte) (string, error) {
	var localStdin io.WriteCloser
	var localScanner *bufio.Scanner
	var isDead bool

	// 先檢查進程狀態
	p.mu.Lock()
	isDead = p.dead
	localStdin = p.stdin
	localScanner = p.stdout
	p.mu.Unlock()

	if isDead {
		return "", fmt.Errorf("ocr worker is dead")
	}

	// 發送請求（stdin 寫入不需要 mutex，因為只有一個 goroutine 會寫）
	imgB64 := base64.StdEncoding.EncodeToString(imgData)
	req := map[string]string{"image": imgB64}
	reqJSON, _ := json.Marshal(req)
	reqJSON = append(reqJSON, '\n')

	if _, err := localStdin.Write(reqJSON); err != nil {
		return "", fmt.Errorf("write to ocr worker: %w", err)
	}

	// 讀取回應（stdout 讀取需要 mutex 保護，但這裡只有一個調用者）
	if !localScanner.Scan() {
		if err := localScanner.Err(); err != nil {
			return "", fmt.Errorf("read from ocr worker: %w", err)
		}
		return "", fmt.Errorf("ocr worker closed unexpectedly")
	}

	var resp ocrWorkerResponse
	if err := json.Unmarshal(localScanner.Bytes(), &resp); err != nil {
		return "", fmt.Errorf("parse ocr response: %w", err)
	}

	if !resp.Success {
		return "", fmt.Errorf("ocr error: %s", resp.Error)
	}

	return resp.Text, nil
}

// close 關閉子行程。
func (p *ocrProcess) close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = p.stdin.Close()
	return nil // Wait 由 goroutine 處理
}

// 全局 OCR 進程實例，由 main 初始化
var globalOcrProcess *ocrProcess

func main() {
	loadDotEnv(".env")
	maxChatRequestBodyBytes = loadMaxChatRequestBodyBytes()
	log.Printf("max chat request body: %d bytes (%.1f MiB)", maxChatRequestBodyBytes, float64(maxChatRequestBodyBytes)/float64(1<<20))

	// 初始化 OCR 子行程（可選，需有 Python + pip install rapidocr）
	var ocrErr error
	globalOcrProcess, ocrErr = startOcrProcess()
	if ocrErr != nil {
		log.Printf("[ocr] OCR worker not started: %v (OCR disabled)", ocrErr)
		globalOcrProcess = nil
	} else {
		defer globalOcrProcess.close()
	}

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

	cacheTTL := parseTTL(strings.TrimSpace(os.Getenv("DS_CACHE_TTL")))
	cache := newThinkingCache(cacheTTL)
	defer cache.close()
	queueTTL := parseTTL(strings.TrimSpace(os.Getenv("DS_QUEUE_TTL")))
	orderQueues := newReasoningOrderQueues(queueTTL)
	defer orderQueues.close()

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = upstream.Scheme
			req.URL.Host = upstream.Host
			req.Host = upstream.Host
			req.Header.Del("Accept-Encoding")
		},
		Transport: &reasoningCacheTransport{
			rt:          http.DefaultTransport,
			cache:       cache,
			orderQueues: orderQueues,
		},
		ModifyResponse: func(resp *http.Response) error {
			// 在響應中設置 conv_id cookie，讓 Cursor 的 HTTP 客戶端在後續請求帶回
			if resp.Request != nil {
				if convKey, ok := resp.Request.Context().Value(ctxKeyConv{}).(string); ok && convKey != "" {
					cookieStr := makeConvCookie(convKey)
					resp.Header.Add("Set-Cookie", cookieStr)
					log.Printf("[resp] Set-Cookie conv_id=%s", convKey[:min(len(convKey), 8)])
				}
			}
			// Fix #2：只對 chat/completions 進行讀取與緩存，跳過其他 API（如 /v1/models）
			if resp.Request == nil || !strings.HasSuffix(resp.Request.URL.Path, "/chat/completions") {
				return nil
			}
			// SSE 回應由 Transport 的 TeeReader 在關閉時緩存，避免阻塞流式輸出
			ct := resp.Header.Get("Content-Type")
			if strings.Contains(ct, "text/event-stream") {
				return nil
			}
			// Fix #6：限制 response body 大小
			limitedBody := io.LimitReader(resp.Body, maxResponseBodySize)
			bodyBytes, err := io.ReadAll(limitedBody)
			if err != nil {
				resp.Body.Close()
				return err
			}
			resp.Body.Close()
			// Fix #1：先檢查 err，再關閉 body
			var cr chatCompletionResponse
			if err := json.Unmarshal(bodyBytes, &cr); err != nil {
				resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				return nil
			}
			// 從請求 context 中取出 conversation key
			convKey, _ := resp.Request.Context().Value(ctxKeyConv{}).(string)
			for _, choice := range cr.Choices {
				m := choice.Message
				if m == nil || m.ReasoningContent == "" {
					continue
				}
				plain := normalizeTextForCache(flattenContentField(m.Content))
				recordAssistantReasoning(cache, orderQueues, convKey, plain, m.ReasoningContent)
			}
			resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			resp.ContentLength = int64(len(bodyBytes))
			resp.Header.Set("Content-Length", strconv.Itoa(len(bodyBytes)))
			resp.Header.Del("Transfer-Encoding")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, e error) {
			log.Printf("proxy error: %v", e)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream error","type":"proxy_error"}}`))
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		withCORS(w, r, func() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		withCORS(w, r, func() {
			path := r.URL.Path
			if r.Method == http.MethodPost && path == "/v1/chat/completions" {
				// 含圖片 base64 的請求體很大，見 maxChatRequestBodyBytes / DS_MAX_REQUEST_BODY
				r.Body = http.MaxBytesReader(w, r.Body, maxChatRequestBodyBytes)
				if err := rewriteChatCompletionBody(r, modelMap, dsChatOpts, cache, orderQueues); err != nil {
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
			proxy.ServeHTTP(w, r)
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

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("received signal %v, shutting down...", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			log.Printf("graceful shutdown: %v", err)
		}
		cache.close()
		orderQueues.close()
	}()

	log.Printf("listening %s -> %s", listen, upstream.String())
	if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// loadDotEnv 讀取 .env 檔案並載入到 os.Environ()，但不覆蓋已存在的環境變數。
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // 檔案不存在也非錯誤
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		eq := bytes.IndexByte(line, '=')
		if eq < 1 {
			continue
		}
		key := strings.TrimSpace(string(line[:eq]))
		val := strings.TrimSpace(string(line[eq+1:]))
		if key == "" {
			continue
		}
		// 不覆蓋已存在的環境變數（os.Environ() 優先）
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}

func parseTTL(s string) time.Duration {
	if s == "" {
		return 24 * time.Hour
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		log.Printf("invalid TTL %q, using default 24h", s)
		return 24 * time.Hour
	}
	if d < 1*time.Minute {
		d = 1 * time.Minute
	}
	return d
}

// buildModelMap 將 Cursor / OpenAI 客戶端常用 model 名稱對應到 DeepSeek。
// MAPPED_MODEL 預設 deepseek-v4-pro；若要改回 flash 可設 MAPPED_MODEL=deepseek-v4-flash。
func buildModelMap() map[string]string {
	target := strings.TrimSpace(os.Getenv("MAPPED_MODEL"))
	if target == "" {
		target = "deepseek-v4-pro"
	}
	m := map[string]string{
		"gpt-4o":                     target,
		"gpt-4o-mini":                target,
		"gpt-4":                      target,
		"gpt-4-turbo":                target,
		"gpt-3.5-turbo":              target,
		"chatgpt-4o-latest":          target,
		"deepseek-v4-pro":            "deepseek-v4-pro",
		"deepseek-v4-pro-thinking":   "deepseek-v4-pro",
		"deepseek-v4-flash":          "deepseek-v4-flash",
		"deepseek-v4-flash-thinking": "deepseek-v4-flash",
	}
	if raw := strings.TrimSpace(os.Getenv("MODEL_MAP")); raw != "" {
		parts := strings.Split(raw, ",")
		for _, p := range parts {
			kv := strings.SplitN(strings.TrimSpace(p), "=", 2)
			if len(kv) != 2 {
				log.Printf("invalid MODEL_MAP entry (expected key=value): %q", p)
				continue
			}
			from, to := strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])
			if from == "" {
				log.Printf("invalid MODEL_MAP entry (empty key): %q", p)
				continue
			}
			if to == "" {
				log.Printf("invalid MODEL_MAP entry (empty value): %q", p)
				continue
			}
			m[from] = to
		}
	}
	if _, exists := m[target]; !exists {
		log.Printf("MAPPED_MODEL=%q is not in model map, requests may fail if upstream rejects it", target)
	}
	return m
}

type deepSeekChatOptions struct {
	reasoningEffort string
}

// loadDeepSeekChatOptions 載入推理相關設定（目前僅 DS_REASONING_EFFORT）。
// Thinking 模式的啟用改由模型名稱後綴 -thinking 控制，不再需要全域環境變數。
func loadDeepSeekChatOptions() deepSeekChatOptions {
	o := deepSeekChatOptions{}
	if v := strings.TrimSpace(os.Getenv("DS_REASONING_EFFORT")); v != "" {
		o.reasoningEffort = v
	}
	return o
}

func rewriteChatCompletionBody(r *http.Request, modelMap map[string]string, opts deepSeekChatOptions, cache *thinkingCache, orderQueues *reasoningOrderQueues) error {
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

	// DS_DEBUG 時輸出原始請求體（截斷避免 log 過長）
	if os.Getenv("DS_DEBUG") == "true" {
		debugBody := body
		if len(debugBody) > 2000 {
			debugBody = debugBody[:2000]
		}
		log.Printf("[debug] original request body: %s", string(debugBody))
	}

	changed := false
	var originalModel string

	// 1. 模型名稱映射
	if rawModel, ok := payload["model"]; ok {
		var modelStr string
		if err := json.Unmarshal(rawModel, &modelStr); err == nil {
			originalModel = modelStr
			if repl, ok := modelMap[modelStr]; ok {
				newRaw, err := json.Marshal(repl)
				if err != nil {
					return err
				}
				payload["model"] = newRaw
				changed = true
				log.Printf("[chat] model: %q → %q", originalModel, repl)
			} else {
				log.Printf("[chat] model: %q (no mapping, passed through)", originalModel)
			}
		}
	}

	// 2. Thinking 模式處理（-thinking 後綴的模型強制啟用；其餘一律禁用）
	perRequestThinking := strings.HasSuffix(originalModel, "-thinking")

	if perRequestThinking {
		thinkingObj, err := json.Marshal(map[string]string{"type": "enabled"})
		if err != nil {
			return err
		}
		payload["thinking"] = thinkingObj
		if _, exists := payload["reasoning_effort"]; !exists {
			effort := opts.reasoningEffort
			if effort == "" {
				effort = "high"
			}
			raw, err := json.Marshal(effort)
			if err != nil {
				return err
			}
			payload["reasoning_effort"] = raw
		}
		changed = true
		log.Printf("[chat] thinking=enabled (per-request, model suffix -thinking)")
	} else {
		dis, err := json.Marshal(map[string]string{"type": "disabled"})
		if err != nil {
			return err
		}
		payload["thinking"] = dis
		delete(payload, "reasoning_effort")
		changed = true
		log.Printf("[chat] thinking=disabled (forced by proxy)")
	}

	// 清理 messages 中的多媒體內容（如 image_url），DeepSeek 不支援非 text content parts
	if rawMsgs, ok := payload["messages"]; ok {
		var msgs []json.RawMessage
		if json.Unmarshal(rawMsgs, &msgs) == nil {
			sanitized := false
			for i := range msgs {
				cleaned, changed := sanitizeMessageContent(msgs[i])
				if changed {
					msgs[i] = cleaned
					sanitized = true
				}
			}
			if sanitized {
				encoded, err := json.Marshal(msgs)
				if err == nil {
					payload["messages"] = encoded
					changed = true
					log.Printf("[chat] processed image_url content parts via OCR")
				}
			}
		}
	}

	// 讀取 cookie 中的 conversation ID（不論是否 thinking 模式）
	var convKey string
	var convFromCookie bool
	if rawMsgs, _ := payload["messages"]; rawMsgs != nil {
		var msgs []json.RawMessage
		if err := json.Unmarshal(rawMsgs, &msgs); err == nil {
			if cookie, err := r.Cookie("conv_id"); err == nil && cookie.Value != "" {
				convKey = cookie.Value
				convFromCookie = true
				log.Printf("[chat] got conv_id from cookie: %s", convKey[:min(len(convKey), 8)])
			} else {
				convKey = computeConversationKey(msgs)
				if err != nil {
					log.Printf("[chat] no conv_id cookie (err=%v), using computed key: %s", err, convKey[:min(len(convKey), 8)])
				} else {
					log.Printf("[chat] conv_id cookie is empty, using computed key: %s", convKey[:min(len(convKey), 8)])
				}
			}
		}
	}
	if convKey != "" {
		ctx := context.WithValue(r.Context(), ctxKeyConv{}, convKey)
		ctx = context.WithValue(ctx, ctxKeyConvFromCookie{}, convFromCookie)
		*r = *r.WithContext(ctx)
	}

	// 3. 補回 reasoning_content（per-request Thinking 模式啟用時）
	if perRequestThinking {
		if rawMsgs, ok := payload["messages"]; ok {
			var msgs []json.RawMessage
			if err := json.Unmarshal(rawMsgs, &msgs); err == nil {
				msgChanged := false
				var needIdx []int
				for i, rawMsg := range msgs {
					if !assistantMessageNeedsReasoningInject(rawMsg) {
						continue
					}
					needIdx = append(needIdx, i)
				}
				k := len(needIdx)

				// convKey 已在上方統一讀取
				snap := orderQueues.lastK(convKey, k)
				offset := 0
				if snap != nil && len(snap) < k {
					offset = k - len(snap)
				}
				hitHash, hitOrder := 0, 0
				var missDetails []string
				for ord, i := range needIdx {
					rawMsg := msgs[i]
					_, plain := assistantPlainTextFromMessageRaw(rawMsg)
					var rc string
					var found bool
					var hitSource string
					if plain != "" {
						rc, found = cache.get(plain)
						if found {
							hitSource = "hash"
						}
					}
					if !found && snap != nil && ord >= offset {
						si := ord - offset
						if si < len(snap) && snap[si].reasoning != "" {
							rc = snap[si].reasoning
							found = true
							hitSource = "order"
						}
					}
					if found {
						merged, err := mergeReasoningContent(rawMsg, rc)
						if err == nil {
							msgs[i] = merged
							msgChanged = true
							if hitSource == "hash" {
								hitHash++
							} else {
								hitOrder++
							}
						}
					} else {
						// 記錄 miss 詳細信息以便除錯
						detail := fmt.Sprintf("  miss msg[%d]: plain_empty=%v snap_avail=%v ord_ge_offset=%v snap_len=%d k=%d offset=%d",
							i, plain == "", snap != nil, ord >= offset,
							func() int {
								if snap != nil {
									return len(snap)
								}
								return -1
							}(), k, offset)
						if plain != "" {
							// 截斷過長的 content 以便可讀
							truncated := plain
							if len(truncated) > 80 {
								truncated = truncated[:80] + "..."
							}
							detail += fmt.Sprintf(" plain=%q", truncated)
						}
						missDetails = append(missDetails, detail)
					}
				}
				missCount := k - hitHash - hitOrder
				if k > 0 {
					cookieTag := ""
					if convFromCookie {
						cookieTag = " cookie"
					}
					convIDShort := ""
					if len(convKey) > 8 {
						convIDShort = convKey[:8]
					} else {
						convIDShort = convKey
					}
					log.Printf("[chat] reasoning bridge: need=%d hash_hit=%d order_hit=%d miss=%d conv=%s%s", k, hitHash, hitOrder, missCount, convIDShort, cookieTag)
					for _, d := range missDetails {
						log.Print(d)
					}
				}
				if msgChanged {
					encoded, err := json.Marshal(msgs)
					if err != nil {
						return err
					}
					payload["messages"] = encoded
					changed = true
				}
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

	// DS_DEBUG 時輸出最終請求體（截斷避免 log 過長）
	if os.Getenv("DS_DEBUG") == "true" {
		debugOut := out
		if len(debugOut) > 2000 {
			debugOut = debugOut[:2000]
		}
		log.Printf("[debug] final request body: %s", string(debugOut))
	}

	r.Body = io.NopCloser(bytes.NewReader(out))
	r.ContentLength = int64(len(out))
	r.Header.Set("Content-Length", strconv.Itoa(len(out)))
	r.Header.Del("Transfer-Encoding")
	return nil
}

// mergeReasoningContent 把 reasoning_content 注入到 assistant 消息的 JSON 中。
func mergeReasoningContent(raw json.RawMessage, reasoning string) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	rc, err := json.Marshal(reasoning)
	if err != nil {
		return nil, err
	}
	m["reasoning_content"] = rc
	return json.Marshal(m)
}

func jsonErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"message": msg,
			"type":    "invalid_request_error",
			"code":    strconv.Itoa(status),
		},
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

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
	written    bool
}

func (w *loggingResponseWriter) WriteHeader(code int) {
	if !w.written {
		w.statusCode = code
		w.written = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *loggingResponseWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.statusCode = http.StatusOK
		w.written = true
	}
	return w.ResponseWriter.Write(b)
}

func logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &loggingResponseWriter{ResponseWriter: w}
		next.ServeHTTP(lw, r)
		duration := time.Since(start)
		status := lw.statusCode
		if status == 0 {
			status = http.StatusOK
		}
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, status, duration.Round(time.Millisecond))
	})
}
