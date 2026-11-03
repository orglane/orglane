-- name: InsertAuditEntry :one
INSERT INTO audit_entries (actor_kind, actor_id, action, target_kind, target_id, metadata, ip, user_agent)
VALUES (@actor_kind, @actor_id, @action, @target_kind, @target_id, @metadata, @ip, @user_agent)
RETURNING *;

