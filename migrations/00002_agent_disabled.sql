-- agents.disabled 持久化（TODO#1 关闭）：停用状态此前仅存 registry 内存，重启后丢失。
-- 零值 0 = 未停用，存量行天然兼容。

-- +goose Up
ALTER TABLE agents ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE agents DROP COLUMN disabled;
