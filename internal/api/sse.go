package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/dddpeter/agent-nexus/internal/bus"
)

// sseReplayLimit 断线补偿重放上限（F5.3：重放最近 500 条）。
const sseReplayLimit = 500

// sseHeartbeat SSE 心跳注释行间隔（F5.4：每 15s 保活）。
const sseHeartbeat = 15 * time.Second

// handleGlobalStream GET /api/stream —— 全局 SSE（agent 状态、消息、任务生命周期）。
// 断线重连：Last-Event-ID（event_log.seq）→ 补偿重放 (lastID, watermark] 再接实时流；
// 通过订阅后取水位线 + 序号去重保证「不丢消息、不重复」（U-F5-03）。
func (s *Server) handleGlobalStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}

	lastID := parseLastEventID(r)
	events := s.deps.Bus

	// 1) 先订阅实时流（避免重放查询与订阅间隙丢事件）
	ch, unsubscribe := events.Subscribe()
	defer unsubscribe()

	// 2) 水位线：此刻已分配的最大 seq（重放上界）
	watermark := events.CurrentSeq()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// 3) DB 补偿重放 (lastID, watermark]
	sent := int64(0)
	if lastID < watermark {
		replay, err := s.deps.Store.Events().EventsAfter(r.Context(), lastID, sseReplayLimit)
		if err != nil {
			s.deps.Log.Error("sse replay query failed", "err", err)
		}
		for _, e := range replay {
			if e.Seq > watermark {
				break
			}
			writeSSE(w, flusher, e)
			sent = e.Seq
		}
	}
	if sent < lastID {
		sent = lastID // 防御：重放为空时水位从 lastID 起
	}

	// 4) 实时事件（seq > sent 才发，避免与重放重复）
	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return
			}
			if e.Seq <= sent {
				continue // 重放已覆盖（订阅早于水位线读取的旧事件）
			}
			writeSSE(w, flusher, e)
			sent = e.Seq
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n") // 注释行保活
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// parseLastEventID 从 Last-Event-ID 头或 last_event_id 查询参数取断线位点。
func parseLastEventID(r *http.Request) int64 {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("last_event_id")
	}
	var id int64
	if raw != "" {
		_, _ = fmt.Sscanf(raw, "%d", &id)
	}
	return id
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, e bus.Event) {
	payload := json.RawMessage(e.Payload)
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Type, payload)
	flusher.Flush()
}
