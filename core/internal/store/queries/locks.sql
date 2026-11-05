-- name: AcquireXactLock :exec
-- Takes a transaction-scoped advisory lock, released at commit or rollback.
SELECT pg_advisory_xact_lock(@lock_id::bigint);

