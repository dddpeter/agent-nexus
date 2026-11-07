package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/dddpeter/agent-nexus/internal/bus"
)

// handleGroupStream GET /api/groups/{id}/stream —— 群独立 SSE（FR5.2）。
// 订阅全局总线，仅转发与该群相关的事件：群消息（payload.group_id 匹配）与
// 该群的仲裁决策（arbiter.decision）；Last-Event-ID 补偿重放与全局流同水位语义。
func (s *Server) handleGroupStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}
	groupID := chi.URLParam(r, "id")

	lastID := parseLastEventID(r)
	ch, unsubscribe := s.deps.Bus.Subscribe()
	defer unsubscribe()
	watermark := s.deps.Bus.CurrentSeq()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// 补偿重放 (lastID, watermark]（过滤群相关）
	if lastID < watermark {
		replay, err := s.deps.Store.Events().EventsAfter(r.Context(), lastID, sseReplayLimit)
		if err != nil {
			s.deps.Log.Error("group stream replay failed", "err", err)
		}
		for _, e := range replay {
			if e.Seq > watermark {
				break
			}
			if groupEventMatches(e, groupID) {
				writeSSE(w, flusher, e)
			}
		}
	}

	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return
			}
			if e.Seq <= lastID {
				continue // 重放已覆盖
			}
			if !groupEventMatches(e, groupID) {
				continue
			}
			writeSSE(w, flusher, e)
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// groupEventMatches 事件是否属于该群：msg.* 群消息与 arbiter.decision 负载均带 group_id。
func groupEventMatches(e bus.Event, groupID string) bool {
	if e.Type != "msg.human" && e.Type != "msg.agent" && e.Type != "msg.system" && e.Type != "arbiter.decision" {
		return false
	}
	var p struct {
		GroupID string `json:"group_id"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return false
	}
	return p.GroupID == groupID
}
