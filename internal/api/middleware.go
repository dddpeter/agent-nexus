package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dddpeter/agent-nexus/internal/repo"
)

// 统一错误结构 {"error":{code,message,request_id}}（F5.4 / U-F5-04）。
type errBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

type errorEnvelope struct {
	Error errBody `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError 统一错误出口。
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{Error: errBody{
		Code: code, Message: message, RequestID: requestIDOf(r),
	}})
}

// mapWriteError 将领域错误映射为 HTTP 状态码。
func mapWriteError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, repo.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, repo.ErrDuplicateName):
		writeError(w, r, http.StatusConflict, "duplicate_name", err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, r, http.StatusGatewayTimeout, "timeout", err.Error())
	default:
		writeError(w, r, http.StatusInternalServerError, "internal", err.Error())
	}
}

// ---------- requestID ----------

type ctxKeyRequestID struct{}

const headerRequestID = "X-Request-Id"

func middlewareRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(headerRequestID)
		if id == "" {
			id = newReqID()
		}
		w.Header().Set(headerRequestID, id)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requestIDOf(r *http.Request) string {
	if v, ok := r.Context().Value(ctxKeyRequestID{}).(string); ok {
		return v
	}
	return ""
}

var reqIDCounter struct {
	mu sync.Mutex
	n  uint64
}

func newReqID() string {
	reqIDCounter.mu.Lock()
	reqIDCounter.n++
	n := reqIDCounter.n
	reqIDCounter.mu.Unlock()
	ts := time.Now().UnixMilli()
	return formatReqID(ts, n)
}

func formatReqID(ts int64, n uint64) string {
	const base36 = "0123456789abcdefghijklmnopqrstuvwxyz"
	buf := make([]byte, 0, 12)
	// 时间戳 36 进制 + 计数器 36 进制，紧凑即可
	for _, v := range []int64{ts % 2176782336, int64(n % 2176782336)} {
		for i := 0; i < 6; i++ {
			buf = append(buf, base36[v%36])
			v /= 36
		}
	}
	return string(buf)
}

// ---------- recover ----------

func middlewareRecover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic recovered", "panic", rec, "path", r.URL.Path)
					writeError(w, r, http.StatusInternalServerError, "panic", "internal server error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// ---------- 鉴权（单用户；双通道：Bearer 供 API/CLI，cookie 会话供浏览器，FR5.1 / TODO#2）----------

func middlewareAuth(token string, sessions *sessionStore, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 免鉴权：健康检查、登录/状态查询；非 /api 路径为 SPA 静态 shell（不含数据）
			if r.URL.Path == "/healthz" ||
				r.URL.Path == "/api/auth/login" || r.URL.Path == "/api/auth/status" ||
				!strings.HasPrefix(r.URL.Path, "/api") {
				next.ServeHTTP(w, r)
				return
			}
			if token == "" {
				next.ServeHTTP(w, r) // 未配置 token：本地开发模式放行（启动时已告警）
				return
			}
			// Bearer（API/CLI）
			if r.Header.Get("Authorization") == "Bearer "+token {
				next.ServeHTTP(w, r)
				return
			}
			// 会话 cookie（SPA / EventSource）
			if c, err := r.Cookie(sessionCookie); err == nil && sessions.valid(c.Value) {
				next.ServeHTTP(w, r)
				return
			}
			writeError(w, r, http.StatusUnauthorized, "unauthorized", "missing or invalid credentials")
		})
	}
}

// ---------- 限流（简单窗口计数，60 req/min，U-F5-02）----------

type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	start  time.Time
	count  int
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, start: time.Now()}
}

func (l *rateLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.start) >= l.window {
		l.start = now
		l.count = 0
	}
	l.count++
	return l.count <= l.limit
}

func middlewareRateLimit(l *rateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !l.allow() {
				writeError(w, r, http.StatusTooManyRequests, "rate_limited", "rate limit exceeded (60 req/min)")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ---------- 日志 ----------

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func middlewareLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			log.Info("http",
				"method", r.Method, "path", r.URL.Path,
				"status", rec.status, "duration_ms", time.Since(start).Milliseconds(),
				"request_id", requestIDOf(r))
		})
	}
}
