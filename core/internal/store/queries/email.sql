-- name: CreateOutboxEmail :one
INSERT INTO email_outbox (id, kind, recipient, sealed_message, max_attempts)
VALUES (@id, @kind, @recipient, @sealed_message, @max_attempts)
RETURNING *;

-- name: GetOutboxEmail :one
SELECT * FROM email_outbox WHERE id = @id;

-- name: GetOutboxEmailForUpdate :one
SELECT * FROM email_outbox WHERE id = @id FOR UPDATE;

-- name: MarkOutboxEmailSent :exec
UPDATE email_outbox
SET status = 'sent', attempts = @attempts, sent_at = now(), sealed_message = NULL, last_error = NULL, updated_at = now()
WHERE id = @id AND status = 'queued';

-- name: RecordOutboxEmailFailure :exec
-- Records a failed attempt; the last one gives up and clears the message.
UPDATE email_outbox
SET attempts = @attempts,
    last_error = @last_error,
    status = CASE WHEN @final::boolean THEN 'failed' ELSE 'queued' END,
    sealed_message = CASE WHEN @final::boolean THEN NULL ELSE sealed_message END,
    updated_at = now()
WHERE id = @id AND status = 'queued';

-- name: CancelOutboxEmail :execrows
UPDATE email_outbox
SET status = 'canceled', sealed_message = NULL, updated_at = now()
WHERE id = @id AND status = 'queued';
