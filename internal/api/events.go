package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/dddpeter/agent-nexus/internal/bus"
)

// handleListEvents GET /api/events?limit=100 —— 事件总线落库存档的检索视图（可观测性）。
// 返回最近 limit 条（seq 降序，最新在前）；payload 为原始 JSON 透传。
func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	if typ := r.URL.Query().Get("type"); typ != "" {
		s.writeEventsByType(w, r, typ, limit)
		return
	}

	maxSeq, err := s.deps.Store.Events().MaxEventSeq(r.Context())
	if err != nil {
		mapWriteError(w, r, err)
		return
	}
	after := maxSeq - int64(limit)
	if after < 0 {
		after = 0
	}
	evs, err := s.deps.Store.Events().EventsAfter(r.Context(), after, limit)
	if err != nil {
		mapWriteError(w, r, err)
		return
	}
	// EventsAfter 升序 → 翻转最新在前
	out := make([]busEventDTO, 0, len(evs))
	for i := len(evs) - 1; i >= 0; i-- {
		out = append(out, toEventDTO(evs[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

// writeEventsByType 按类型过滤（小库直查最近 500 条再过滤，事件量级一期可接受）。
func (s *Server) writeEventsByType(w http.ResponseWriter, r *http.Request, typ string, limit int) {
	maxSeq, err := s.deps.Store.Events().MaxEventSeq(r.Context())
	if err != nil {
		mapWriteError(w, r, err)
		return
	}
	after := maxSeq - 500
	if after < 0 {
		after = 0
	}
	evs, err := s.deps.Store.Events().EventsAfter(r.Context(), after, 500)
	if err != nil {
		mapWriteError(w, r, err)
		return
	}
	out := make([]busEventDTO, 0, limit)
	for i := len(evs) - 1; i >= 0 && len(out) < limit; i-- {
		if evs[i].Type == typ {
			out = append(out, toEventDTO(evs[i]))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// busEventDTO 事件 DTO（与 bus.Event JSON 契约一致；SSE 帧同构）。
type busEventDTO struct {
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt string          `json:"created_at"`
}

func toEventDTO(e bus.Event) busEventDTO {
	return busEventDTO{Seq: e.Seq, Type: e.Type, Payload: e.Payload, CreatedAt: e.CreatedAt.Format(time.RFC3339Nano)}
}
