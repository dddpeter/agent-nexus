-- Nexus 初始 schema（严格按 technical-design §3 DDL）

-- +goose Up
CREATE TABLE agents (
  id TEXT PRIMARY KEY,            -- nanoid
  name TEXT UNIQUE NOT NULL,
  adapter TEXT NOT NULL,          -- 'hermes' | 'picoclaw' | ...
  config_json TEXT NOT NULL,      -- adapter 专有配置（endpoint/命令行模板等）
  capabilities_json TEXT NOT NULL,-- CanGroupChat/CanStream/MaxConcurrency/...
  status TEXT NOT NULL DEFAULT 'offline',  -- online|offline|busy|removed
  last_heartbeat_at DATETIME
);

CREATE TABLE sessions (
  id TEXT PRIMARY KEY,
  agent_id TEXT REFERENCES agents(id),
  kind TEXT NOT NULL,             -- private|group|task
  external_ref TEXT,              -- agent 侧会话标识（--session id / HTTP session）；群会话（kind=group）此字段为空
  topic_id TEXT,                  -- 群会话的话题标识；私聊/任务会话为空
  created_at DATETIME, closed_at DATETIME
);

CREATE TABLE messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  sender_type TEXT NOT NULL,      -- human|agent|system
  sender_id TEXT NOT NULL,
  content TEXT NOT NULL,
  reply_to TEXT,
  created_at DATETIME NOT NULL
);
CREATE INDEX idx_messages_session ON messages(session_id, created_at);

CREATE TABLE event_log (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  type TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  created_at DATETIME NOT NULL
);

CREATE TABLE groups (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  members_json TEXT NOT NULL,     -- [{id,type,trigger,budget,priority}]
  arbiter_config_json TEXT NOT NULL
);

CREATE TABLE tasks (
  id TEXT PRIMARY KEY,
  workflow_id TEXT,
  agent_id TEXT,
  input TEXT, status TEXT,        -- queued|running|done|failed|cancelled
  result TEXT, error TEXT,
  nodes_json TEXT,                -- 节点级状态（各节点 status/output/error），一期内嵌
  timeout_seconds INT, retries INT,
  created_at DATETIME, finished_at DATETIME
);

-- +goose Down
DROP TABLE tasks;
DROP TABLE groups;
DROP TABLE event_log;
DROP INDEX idx_messages_session;
DROP TABLE messages;
DROP TABLE sessions;
DROP TABLE agents;
