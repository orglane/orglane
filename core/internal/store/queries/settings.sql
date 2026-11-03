-- name: GetInstanceSetting :one
SELECT value FROM instance_settings WHERE key = @key;

-- name: PutInstanceSetting :exec
INSERT INTO instance_settings (key, value) VALUES (@key, @value)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();

-- name: DeleteInstanceSetting :execrows
DELETE FROM instance_settings WHERE key = @key;
