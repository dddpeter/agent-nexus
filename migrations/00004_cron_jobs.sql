-- cron_jobs（F4.3 定时任务）：cron 表达式触发 chain 或单 agent 调用。
-- builtin 降级语义：调度循环重启恢复（enabled 任务继续调度）；触发时 agent offline 跳过并记录。

-- +goose Up
CREATE TABLE cron_jobs (
  id TEXT PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,
  cron_expr TEXT NOT NULL,        -- 5 字段：分 时 日 月 周
  target_json TEXT NOT NULL,      -- {"type":"chain","chain":{...},"input":...} | {"type":"agent","agent_id":...,"input":...}
  enabled INTEGER NOT NULL DEFAULT 1,
  last_run_at DATETIME,
  last_status TEXT,               -- ok|skipped_offline|failed
  last_task_id TEXT,              -- 最近一次触发的任务 id
  created_at DATETIME NOT NULL
);

-- +goose Down
DROP TABLE cron_jobs;
