package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/dddpeter/agent-nexus/internal/orchestr"
	"github.com/dddpeter/agent-nexus/internal/repo"
)

// taskDTO 任务视图（含节点级状态）。
type taskDTO struct {
	ID        string            `json:"id"`
	Status    string            `json:"status"`
	Input     string            `json:"input,omitempty"`
	Result    string            `json:"result,omitempty"`
	Error     string            `json:"error,omitempty"`
	Nodes     []json.RawMessage `json:"nodes,omitempty"`
	CreatedAt string            `json:"created_at,omitempty"`
	Finished  string            `json:"finished_at,omitempty"`
}

func toTaskDTO(t repo.Task) taskDTO {
	out := taskDTO{ID: t.ID, Status: t.Status}
	if t.Input != nil {
		out.Input = *t.Input
	}
	if t.Result != nil {
		out.Result = *t.Result
	}
	if t.Error != nil {
		out.Error = *t.Error
	}
	if t.NodesJSON != nil {
		_ = json.Unmarshal([]byte(*t.NodesJSON), &out.Nodes)
	}
	if t.CreatedAt != nil {
		out.CreatedAt = t.CreatedAt.UTC().Format(millis)
	}
	if t.FinishedAt != nil {
		out.Finished = t.FinishedAt.UTC().Format(millis)
	}
	return out
}

// handleCreateTask POST /api/tasks：{chain:{nodes:[...],input}} 提交协作链；{cron:{...}} 登记定时任务。
func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	if s.deps.Orchestr == nil {
		writeError(w, r, http.StatusNotImplemented, "not_available", "任务编排器未启用")
		return
	}
	var req struct {
		Chain *orchestr.ChainRequest `json:"chain"`
		Cron  *struct {
			Name   string              `json:"name"`
			Expr   string              `json:"expr"`
			Target orchestr.CronTarget `json:"target"`
		} `json:"cron"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.Chain == nil && req.Cron == nil) {
		writeError(w, r, http.StatusBadRequest, "bad_request", "body 需要 {chain:{nodes:[...]}} 或 {cron:{name,expr,target}}")
		return
	}
	switch {
	case req.Chain != nil:
		id, err := s.deps.Orchestr.SubmitChain(r.Context(), *req.Chain)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "status": repo.TaskQueued})
	case req.Cron != nil:
		view, err := s.deps.Orchestr.CreateCronJob(r.Context(), req.Cron.Name, req.Cron.Expr, req.Cron.Target)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, view)
	}
}

// handleListTasks GET /api/tasks?limit=&status=
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	if s.deps.Orchestr == nil {
		writeJSON(w, http.StatusOK, []taskDTO{})
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	tasks, err := s.deps.Store.Tasks().List(r.Context())
	if err != nil {
		mapWriteError(w, r, err)
		return
	}
	status := r.URL.Query().Get("status")
	out := make([]taskDTO, 0, limit)
	for _, t := range tasks {
		if status != "" && t.Status != status {
			continue
		}
		out = append(out, toTaskDTO(t))
		if len(out) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetTask GET /api/tasks/{id}
func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	t, err := s.deps.Store.Tasks().Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		mapWriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toTaskDTO(t))
}

// handleCancelTask POST /api/tasks/{id}/cancel（FR4.5）
func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	if s.deps.Orchestr == nil {
		writeError(w, r, http.StatusNotImplemented, "not_available", "任务编排器未启用")
		return
	}
	if err := s.deps.Orchestr.Cancel(r.Context(), chi.URLParam(r, "id")); err != nil {
		mapWriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleResumeTask POST /api/tasks/{id}/resume（FR4.5：failed 修参后续跑，builtin 从失败节点继续）
func (s *Server) handleResumeTask(w http.ResponseWriter, r *http.Request) {
	if s.deps.Orchestr == nil {
		writeError(w, r, http.StatusNotImplemented, "not_available", "任务编排器未启用")
		return
	}
	id, err := s.deps.Orchestr.ResumeChain(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			mapWriteError(w, r, err)
			return
		}
		writeError(w, r, http.StatusConflict, "resume_rejected", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "status": repo.TaskQueued})
}

// cronJobDTO 定时任务视图（与 orchestr.CronJobView 同形，独立定义避免 API↔orchestr 越界耦合）。
type cronJobDTO orchestr.CronJobView

// handleListCron GET /api/cron
func (s *Server) handleListCron(w http.ResponseWriter, r *http.Request) {
	if s.deps.Orchestr == nil {
		writeJSON(w, http.StatusOK, []cronJobDTO{})
		return
	}
	jobs, err := s.deps.Orchestr.ListCronJobs(r.Context())
	if err != nil {
		mapWriteError(w, r, err)
		return
	}
	out := make([]cronJobDTO, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, cronJobDTO(j))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDeleteCron DELETE /api/cron/{id}
func (s *Server) handleDeleteCron(w http.ResponseWriter, r *http.Request) {
	if s.deps.Orchestr == nil {
		writeError(w, r, http.StatusNotImplemented, "not_available", "任务编排器未启用")
		return
	}
	if err := s.deps.Orchestr.DeleteCronJob(r.Context(), chi.URLParam(r, "id")); err != nil {
		mapWriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
