package repo

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/dddpeter/agent-nexus/internal/bus"
)

// memStore 是全部聚合 repo 的内存实现（单测用，不碰真 SQLite；test-plan §1）。
type memStore struct {
	mu          sync.RWMutex
	agents      map[string]Agent
	agentByName map[string]string
	sessions    map[string]Session
	messages    []Message
	groups      map[string]Group
	tasks       map[string]Task
	events      []bus.Event
}

// NewMemoryStore 创建内存 Store。
func NewMemoryStore() Store {
	m := &memStore{
		agents:      make(map[string]Agent),
		agentByName: make(map[string]string),
		sessions:    make(map[string]Session),
		groups:      make(map[string]Group),
		tasks:       make(map[string]Task),
	}
	return m
}

func (m *memStore) Agents() AgentRepo     { return (*memAgentRepo)(m) }
func (m *memStore) Sessions() SessionRepo { return (*memSessionRepo)(m) }
func (m *memStore) Messages() MessageRepo { return (*memMessageRepo)(m) }
func (m *memStore) Groups() GroupRepo     { return (*memGroupRepo)(m) }
func (m *memStore) Tasks() TaskRepo       { return (*memTaskRepo)(m) }
func (m *memStore) Events() EventRepo     { return (*memEventRepo)(m) }
func (m *memStore) Close() error          { return nil }

// ---------- agents ----------

type memAgentRepo memStore

func (m *memAgentRepo) Create(_ context.Context, a Agent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.agentByName[a.Name]; dup {
		return fmt.Errorf("%w: %q", ErrDuplicateName, a.Name)
	}
	m.agents[a.ID] = a
	m.agentByName[a.Name] = a.ID
	return nil
}

func (m *memAgentRepo) Get(_ context.Context, id string) (Agent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.agents[id]
	if !ok {
		return Agent{}, fmt.Errorf("%w: agent %s", ErrNotFound, id)
	}
	return a, nil
}

func (m *memAgentRepo) GetByName(_ context.Context, name string) (Agent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.agentByName[name]
	if !ok {
		return Agent{}, fmt.Errorf("%w: agent name %q", ErrNotFound, name)
	}
	return m.agents[id], nil
}

func (m *memAgentRepo) List(_ context.Context, f AgentFilter) ([]Agent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Agent
	for _, a := range m.agents {
		if f.Status != "" && a.Status != f.Status {
			continue
		}
		if f.Adapter != "" && a.Adapter != f.Adapter {
			continue
		}
		if !f.IncludeRemoved && a.Status == StatusRemoved {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (m *memAgentRepo) Update(_ context.Context, a Agent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.agents[a.ID]
	if !ok {
		return fmt.Errorf("%w: agent %s", ErrNotFound, a.ID)
	}
	m.agents[a.ID] = a
	if old.Name != a.Name { // 名字理论不可变，防御性维护索引
		delete(m.agentByName, old.Name)
		m.agentByName[a.Name] = a.ID
	}
	return nil
}

// ---------- sessions ----------

type memSessionRepo memStore

func (m *memSessionRepo) Create(_ context.Context, s Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.ID] = s
	return nil
}

func (m *memSessionRepo) Get(_ context.Context, id string) (Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	if !ok {
		return Session{}, fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	return s, nil
}

func (m *memSessionRepo) Update(_ context.Context, s Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[s.ID]; !ok {
		return fmt.Errorf("%w: session %s", ErrNotFound, s.ID)
	}
	m.sessions[s.ID] = s
	return nil
}

func (m *memSessionRepo) ListRecent(_ context.Context, limit int) ([]Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	all := make([]Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func (m *memSessionRepo) ListByAgent(_ context.Context, agentID string) ([]Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Session
	for _, s := range m.sessions {
		if s.AgentID != nil && *s.AgentID == agentID {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// ---------- messages ----------

type memMessageRepo memStore

func (m *memMessageRepo) Append(_ context.Context, msg *Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	msg.ID = int64(len(m.messages) + 1)
	m.messages = append(m.messages, *msg)
	return nil
}

func (m *memMessageRepo) ListBySession(_ context.Context, sessionID string) ([]Message, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Message
	for _, msg := range m.messages {
		if msg.SessionID == sessionID {
			out = append(out, msg)
		}
	}
	return out, nil
}

func (m *memMessageRepo) RecentBySession(_ context.Context, sessionID string, limit int) ([]Message, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var matched []Message
	for _, msg := range m.messages {
		if msg.SessionID == sessionID {
			matched = append(matched, msg)
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].CreatedAt.After(matched[j].CreatedAt)
		}
		return matched[i].ID > matched[j].ID
	})
	if len(matched) > limit {
		matched = matched[:limit]
	}
	for i, j := 0, len(matched)-1; i < j; i, j = i+1, j-1 {
		matched[i], matched[j] = matched[j], matched[i]
	}
	return matched, nil
}

// ---------- groups ----------

type memGroupRepo memStore

func (m *memGroupRepo) Put(_ context.Context, g Group) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.groups[g.ID] = g
	return nil
}

func (m *memGroupRepo) Get(_ context.Context, id string) (Group, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	g, ok := m.groups[id]
	if !ok {
		return Group{}, fmt.Errorf("%w: group %s", ErrNotFound, id)
	}
	return g, nil
}

func (m *memGroupRepo) List(_ context.Context) ([]Group, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Group
	for _, g := range m.groups {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ---------- tasks ----------

type memTaskRepo memStore

func (m *memTaskRepo) Create(_ context.Context, t Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks[t.ID] = t
	return nil
}

func (m *memTaskRepo) Get(_ context.Context, id string) (Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tasks[id]
	if !ok {
		return Task{}, fmt.Errorf("%w: task %s", ErrNotFound, id)
	}
	return t, nil
}

func (m *memTaskRepo) Update(_ context.Context, t Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[t.ID]; !ok {
		return fmt.Errorf("%w: task %s", ErrNotFound, t.ID)
	}
	m.tasks[t.ID] = t
	return nil
}

func (m *memTaskRepo) List(_ context.Context) ([]Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Task
	for _, t := range m.tasks {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := time.Time{}, time.Time{}
		if t := out[i].CreatedAt; t != nil {
			ti = *t
		}
		if t := out[j].CreatedAt; t != nil {
			tj = *t
		}
		return ti.After(tj)
	})
	return out, nil
}

// ---------- events ----------

type memEventRepo memStore

func (m *memEventRepo) AppendEvents(_ context.Context, events []bus.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, events...)
	return nil
}

func (m *memEventRepo) MaxEventSeq(_ context.Context) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.events) == 0 {
		return 0, nil
	}
	return m.events[len(m.events)-1].Seq, nil
}

func (m *memEventRepo) EventsAfter(_ context.Context, afterSeq int64, limit int) ([]bus.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []bus.Event
	for _, e := range m.events {
		if e.Seq > afterSeq {
			out = append(out, e)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}
