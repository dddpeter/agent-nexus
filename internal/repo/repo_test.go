package repo_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dddpeter/agent-nexus/internal/bus"
	"github.com/dddpeter/agent-nexus/internal/repo"
)

func newMemStore(t *testing.T) repo.Store {
	t.Helper()
	return repo.NewMemoryStore()
}

func newSQLiteStore(t *testing.T) repo.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := repo.OpenSQLite(context.Background(), dir+"/test.db")
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

// 行为一致性用例：内存实现与 SQLite 实现必须全部通过（test-plan §1）。
func stores() map[string]func(t *testing.T) repo.Store {
	return map[string]func(t *testing.T) repo.Store{
		"memory": newMemStore,
		"sqlite": newSQLiteStore,
	}
}

func TestAgentRepoCRUD(t *testing.T) {
	for name, mk := range stores() {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			hb := time.Now().Truncate(time.Millisecond)

			a := repo.Agent{
				ID: "a1", Name: "echod-main", Adapter: "echod",
				ConfigJSON: `{"mode":"echo"}`, CapabilitiesJSON: `{"max_concurrency":2}`,
				Status: repo.StatusOnline, LastHeartbeatAt: &hb,
			}
			require.NoError(t, s.Agents().Create(ctx, a))

			// 重名拒绝（U-F1-03 存储层前置）
			err := s.Agents().Create(ctx, repo.Agent{ID: "a2", Name: "echod-main", Adapter: "echod",
				ConfigJSON: "{}", CapabilitiesJSON: "{}", Status: repo.StatusOffline})
			require.ErrorIs(t, err, repo.ErrDuplicateName)

			got, err := s.Agents().Get(ctx, "a1")
			require.NoError(t, err)
			assert.Equal(t, a, got)

			byName, err := s.Agents().GetByName(ctx, "echod-main")
			require.NoError(t, err)
			assert.Equal(t, "a1", byName.ID)

			// 更新状态 + 心跳
			a.Status = repo.StatusOffline
			require.NoError(t, s.Agents().Update(ctx, a))
			got, _ = s.Agents().Get(ctx, "a1")
			assert.Equal(t, repo.StatusOffline, got.Status)

			// 软删后默认不可见，IncludeRemoved 可见
			a.Status = repo.StatusRemoved
			require.NoError(t, s.Agents().Update(ctx, a))
			list, err := s.Agents().List(ctx, repo.AgentFilter{})
			require.NoError(t, err)
			assert.Empty(t, list)
			list, err = s.Agents().List(ctx, repo.AgentFilter{IncludeRemoved: true})
			require.NoError(t, err)
			assert.Len(t, list, 1)

			_, err = s.Agents().Get(ctx, "nope")
			assert.ErrorIs(t, err, repo.ErrNotFound)
		})
	}
}

func TestSessionRepo(t *testing.T) {
	for name, mk := range stores() {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			require.NoError(t, s.Agents().Create(ctx, repo.Agent{
				ID: "a1", Name: "e", Adapter: "echod", ConfigJSON: "{}",
				CapabilitiesJSON: "{}", Status: repo.StatusOnline,
			}))

			sess := repo.Session{ID: "s1", Kind: repo.KindPrivate, CreatedAt: time.Now()}
			agentID := "a1"
			sess.AgentID = &agentID
			require.NoError(t, s.Sessions().Create(ctx, sess))

			got, err := s.Sessions().Get(ctx, "s1")
			require.NoError(t, err)
			require.NotNil(t, got.AgentID)
			assert.Equal(t, "a1", *got.AgentID)
			assert.Nil(t, got.ExternalRef)

			// 续话映射回填（F2.3）
			ext := "agent-side-42"
			got.ExternalRef = &ext
			require.NoError(t, s.Sessions().Update(ctx, got))
			got, _ = s.Sessions().Get(ctx, "s1")
			require.NotNil(t, got.ExternalRef)
			assert.Equal(t, "agent-side-42", *got.ExternalRef)

			list, err := s.Sessions().ListByAgent(ctx, "a1")
			require.NoError(t, err)
			assert.Len(t, list, 1)

			_, err = s.Sessions().Get(ctx, "nope")
			assert.ErrorIs(t, err, repo.ErrNotFound)
		})
	}
}

// CloseIdleBefore（FR2.5）：按最后活动（末条消息/created_at 回退）批量归档；已关闭不重复。
func TestSessionCloseIdle(t *testing.T) {
	for name, mk := range stores() {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			now := time.Now().Truncate(time.Second)

			idle := repo.Session{ID: "s-idle", Kind: repo.KindPrivate, CreatedAt: now.Add(-48 * time.Hour)}
			fresh := repo.Session{ID: "s-fresh", Kind: repo.KindPrivate, CreatedAt: now.Add(-1 * time.Hour)}
			closed := now.Add(-48 * time.Hour)
			already := repo.Session{ID: "s-closed", Kind: repo.KindPrivate, CreatedAt: closed, ClosedAt: &closed}
			for _, sess := range []repo.Session{idle, fresh, already} {
				require.NoError(t, s.Sessions().Create(ctx, sess))
			}
			// idle 的末条消息 30h 前；fresh 的消息 10min 前
			require.NoError(t, s.Messages().Append(ctx, &repo.Message{
				SessionID: "s-idle", SenderType: repo.SenderHuman, Content: "old", CreatedAt: now.Add(-30 * time.Hour)}))
			require.NoError(t, s.Messages().Append(ctx, &repo.Message{
				SessionID: "s-fresh", SenderType: repo.SenderHuman, Content: "new", CreatedAt: now.Add(-10 * time.Minute)}))

			ids, err := s.Sessions().CloseIdleBefore(ctx, now.Add(-24*time.Hour), now)
			require.NoError(t, err)
			assert.ElementsMatch(t, []string{"s-idle"}, ids)

			got, err := s.Sessions().Get(ctx, "s-idle")
			require.NoError(t, err)
			require.NotNil(t, got.ClosedAt)
			got, _ = s.Sessions().Get(ctx, "s-fresh")
			assert.Nil(t, got.ClosedAt, "活跃会话不受影响")
		})
	}
}

func TestMessageRepo(t *testing.T) {
	for name, mk := range stores() {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			base := time.Now().Truncate(time.Millisecond)

			for i := 0; i < 5; i++ {
				m := repo.Message{
					SessionID: "s1", SenderType: repo.SenderHuman, SenderID: "u1",
					Content: timeNowStr(base, i), CreatedAt: base.Add(time.Duration(i) * time.Second),
				}
				require.NoError(t, s.Messages().Append(ctx, &m))
				require.NotZero(t, m.ID, "Append 应回填自增 ID")
			}
			replyTo := "3"
			require.NoError(t, s.Messages().Append(ctx, &repo.Message{
				SessionID: "s1", SenderType: repo.SenderSystem, SenderID: "system",
				Content: "backfill", ReplyTo: &replyTo, CreatedAt: base.Add(10 * time.Second),
			}))

			all, err := s.Messages().ListBySession(ctx, "s1")
			require.NoError(t, err)
			assert.Len(t, all, 6)

			recent, err := s.Messages().RecentBySession(ctx, "s1", 2)
			require.NoError(t, err)
			require.Len(t, recent, 2)
			// 时间升序，最新在尾
			assert.Equal(t, "backfill", recent[1].Content)
			require.NotNil(t, recent[1].ReplyTo)
			assert.Equal(t, "3", *recent[1].ReplyTo)
		})
	}
}

func timeNowStr(base time.Time, i int) string {
	return base.Add(time.Duration(i) * time.Second).Format(time.RFC3339)
}

func TestEventRepo(t *testing.T) {
	for name, mk := range stores() {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			now := time.Now().Truncate(time.Millisecond)
			evs := []bus.Event{
				{Seq: 1, Type: bus.TypeMsgHuman, Payload: []byte(`{"a":1}`), CreatedAt: now},
				{Seq: 2, Type: bus.TypeMsgAgent, Payload: []byte(`{"a":2}`), CreatedAt: now},
				{Seq: 3, Type: bus.TypeAgentStatus, Payload: []byte(`{"a":3}`), CreatedAt: now},
			}
			require.NoError(t, s.Events().AppendEvents(ctx, evs))

			max, err := s.Events().MaxEventSeq(ctx)
			require.NoError(t, err)
			assert.Equal(t, int64(3), max)

			after, err := s.Events().EventsAfter(ctx, 1, 10)
			require.NoError(t, err)
			require.Len(t, after, 2)
			assert.Equal(t, int64(2), after[0].Seq)
			assert.JSONEq(t, `{"a":2}`, string(after[0].Payload))

			// limit 生效
			lim, err := s.Events().EventsAfter(ctx, 0, 1)
			require.NoError(t, err)
			assert.Len(t, lim, 1)
		})
	}
}

func TestGroupAndTaskRepo(t *testing.T) {
	for name, mk := range stores() {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)

			g := repo.Group{ID: "g1", Name: "room", MembersJSON: `[]`, ArbiterConfigJSON: `{}`}
			require.NoError(t, s.Groups().Put(ctx, g))
			g.Name = "room2"
			require.NoError(t, s.Groups().Put(ctx, g)) // upsert
			got, err := s.Groups().Get(ctx, "g1")
			require.NoError(t, err)
			assert.Equal(t, "room2", got.Name)

			created := time.Now()
			tk := repo.Task{ID: "t1", Status: repo.TaskQueued, CreatedAt: &created}
			require.NoError(t, s.Tasks().Create(ctx, tk))
			tk.Status = repo.TaskRunning
			require.NoError(t, s.Tasks().Update(ctx, tk))
			gotTask, err := s.Tasks().Get(ctx, "t1")
			require.NoError(t, err)
			assert.Equal(t, repo.TaskRunning, gotTask.Status)

			_, err = s.Tasks().Get(ctx, "nope")
			assert.ErrorIs(t, err, repo.ErrNotFound)
		})
	}
}

// U-F6-04 messages 索引查询：万条消息按 session 查最近 20 条 < 10ms。
func TestMessageIndexPerf_U_F6_04(t *testing.T) {
	if testing.Short() {
		t.Skip("perf case skipped in short mode")
	}
	ctx := context.Background()
	s := newSQLiteStore(t)

	// 准备：3 个 session 共 1 万条消息
	txBegin := time.Now()
	const total = 10000
	for i := 0; i < total; i++ {
		m := repo.Message{
			SessionID:  "sess-" + string(rune('a'+i%3)),
			SenderType: repo.SenderHuman, SenderID: "u",
			Content:   "hello world message body",
			CreatedAt: time.Now().Add(time.Duration(i) * time.Millisecond),
		}
		require.NoError(t, s.Messages().Append(ctx, &m))
	}
	t.Logf("setup %d rows in %s", total, time.Since(txBegin))

	// 预热一次（排除首次查询计划编译）
	_, err := s.Messages().RecentBySession(ctx, "sess-a", 20)
	require.NoError(t, err)

	// 多次取最小值，降低调度抖动
	best := time.Duration(1 << 62)
	for i := 0; i < 5; i++ {
		start := time.Now()
		got, err := s.Messages().RecentBySession(ctx, "sess-a", 20)
		require.NoError(t, err)
		require.Len(t, got, 20)
		if el := time.Since(start); el < best {
			best = el
		}
	}
	t.Logf("best query latency: %s", best)
	assert.Less(t, best.Microseconds(), int64(10000), "最近 20 条查询应 < 10ms")
}
