-- groups.state_json（U-F6-05 / TODO#12 关闭）：仲裁器群状态快照 30s 异步落库，重启恢复。
-- 丢失容忍（设计 F3.3）：快照失败仅记日志，不影响群聊主流程。

-- +goose Up
ALTER TABLE groups ADD COLUMN state_json TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE groups DROP COLUMN state_json;
