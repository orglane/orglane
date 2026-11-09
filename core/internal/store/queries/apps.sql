-- App instances ------------------------------------------------------------

-- name: InsertAppInstance :one
INSERT INTO app_instances (key, provider, management, display_name, state, base_url, settings, access_policy, package_id)
VALUES (@key, @provider, @management, @display_name, @state, @base_url, @settings, @access_policy, @package_id)
RETURNING *;

-- name: GetAppInstance :one
SELECT * FROM app_instances WHERE id = @id;

-- name: GetAppInstanceForUpdate :one
-- NO KEY UPDATE: instance IDs never change, so foreign-key checks of child
-- inserts (credentials, webhook deliveries) never wait on this lock
-- (docs/specs/providers.md "Credentials", lock order).
SELECT * FROM app_instances WHERE id = @id FOR NO KEY UPDATE;

-- name: ListAppInstances :many
SELECT * FROM app_instances ORDER BY created_at, id;

-- name: ListAppInstancesInState :many
SELECT * FROM app_instances WHERE state = ANY(@states::text[]) ORDER BY created_at, id;

-- name: UpdateAppInstanceConfig :one
UPDATE app_instances
SET display_name = @display_name, access_policy = @access_policy,
    manage_existing_members = @manage_existing_members, settings = @settings, updated_at = now()
WHERE id = @id
RETURNING *;

-- name: SetAppInstanceState :one
UPDATE app_instances
SET state = @state, health = @health, health_code = @health_code, health_checked_at = now(), updated_at = now()
WHERE id = @id
RETURNING *;

-- Desired specs and operations (managed only) -----------------------------

-- name: InsertAppDesiredSpec :one
INSERT INTO app_desired_specs (instance_id, generation, package_version, run_state, purge, settings, credential_versions, reason)
VALUES (@instance_id, @generation, @package_version, @run_state, @purge, @settings, @credential_versions, @reason)
RETURNING *;

-- name: GetCurrentAppDesiredSpec :one
SELECT * FROM app_desired_specs WHERE instance_id = @instance_id ORDER BY generation DESC LIMIT 1;

-- name: InsertAppOperation :one
INSERT INTO app_operations (instance_id, generation, kind, state, phase, agent_operation_id, from_version, to_version, started_by)
VALUES (@instance_id, @generation, @kind, @state, @phase, @agent_operation_id, @from_version, @to_version, @started_by)
RETURNING *;

-- name: GetAppDesiredSpec :one
SELECT * FROM app_desired_specs WHERE instance_id = @instance_id AND generation = @generation;

-- name: SetAppInstanceDesiredGeneration :execrows
-- Conditional on the generation it replaces, so two writers cannot both
-- record the same next generation.
UPDATE app_instances SET desired_generation = @generation, updated_at = now()
WHERE id = @id AND desired_generation = @previous;

-- name: GetAppPushOperation :one
-- The operation of a pushed desired generation (app_operations_generation_key).
SELECT * FROM app_operations
WHERE instance_id = @instance_id AND generation = @generation AND kind IN ('apply', 'upgrade', 'remove');

-- name: MirrorAppOperation :execrows
-- Records what the agent reports (or a core-side end such as superseded).
-- Zero rows: the operation already finished, so nothing more is recorded
-- and its consequences are not applied twice.
UPDATE app_operations
SET agent_operation_id = @agent_operation_id, state = @state, phase = @phase, error_code = @error_code, finished_at = @finished_at
WHERE id = @id AND finished_at IS NULL;

-- name: SetAppOperationWaiting :execrows
-- Notes why an unfinished operation waits for the agent (agent_unavailable,
-- ...), or clears it (NULL) once the agent answers; a mirror clears it too.
UPDATE app_operations SET error_code = @error_code WHERE id = @id AND finished_at IS NULL;

-- name: ListUnfinishedAppPushOperations :many
SELECT o.instance_id, o.generation::bigint AS generation FROM app_operations o
WHERE o.finished_at IS NULL AND o.generation IS NOT NULL AND o.kind IN ('apply', 'upgrade', 'remove')
ORDER BY o.created_at;

-- name: ListAppOperationHistory :many
SELECT o.*, d.reason AS reason FROM app_operations o
LEFT JOIN app_desired_specs d ON d.instance_id = o.instance_id AND d.generation = o.generation
WHERE o.instance_id = @instance_id
ORDER BY o.created_at DESC, o.id DESC
LIMIT @row_limit OFFSET @row_offset;

-- name: GetLatestAppUpgradeOperation :one
-- The newest upgrade operation, for the automatic planner and the upgrade
-- status in Settings → Apps.
SELECT * FROM app_operations
WHERE instance_id = @instance_id AND kind = 'upgrade'
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: AppUpgradeInProgress :one
-- One managed instance at a time (agent.md "Upgrade"): another instance
-- is upgrading or has an unfinished upgrade operation.
SELECT EXISTS (
    SELECT 1 FROM app_instances i
    WHERE i.management = 'managed' AND i.state = 'upgrading' AND i.id <> @id
) OR EXISTS (
    SELECT 1 FROM app_operations o
    JOIN app_instances i ON i.id = o.instance_id
    WHERE o.kind = 'upgrade' AND o.finished_at IS NULL
      AND i.management = 'managed' AND o.instance_id <> @id
);

-- name: ListAppInstancesWaitingForAgent :many
-- Instances with an unfinished operation that waits for the agent, with
-- the newest reason.
SELECT DISTINCT ON (instance_id) instance_id, error_code::text AS error_code FROM app_operations
WHERE finished_at IS NULL AND error_code = ANY(@codes::text[])
ORDER BY instance_id, created_at DESC;

-- name: SetAppInstanceApplied :exec
-- An operation of generation succeeded. Fenced by generation: an older
-- operation that ends after a newer one was recorded changes nothing.
UPDATE app_instances
SET observed_generation = @generation,
    package_version = coalesce(sqlc.narg(package_version), package_version), updated_at = now()
WHERE id = @id AND observed_generation <= @generation;

-- name: SetAppInstanceObservation :execrows
-- What the agent runs, fenced the same way: a status read before a newer
-- generation was recorded does not move it back. Zero rows: the status is
-- stale, and nothing else may be derived from it. (An agent that lost an
-- instance reports no status for it, so nothing is written.)
UPDATE app_instances
SET observed_generation = @observed_generation,
    package_version = coalesce(sqlc.narg(package_version), package_version), updated_at = now()
WHERE id = @id AND observed_generation <= @observed_generation;

-- name: SetAppInstanceHealth :execrows
-- Conditional on the state the sync read, on the generation the health was
-- observed for (a check of an older generation never lands on a newer one)
-- and on a health code the sync owns, so it never overwrites a reason
-- another path set (setup_failed, webhook_deleted, ...).
UPDATE app_instances SET health = @health, health_code = @health_code, health_checked_at = now(), updated_at = now()
WHERE id = @id AND state = @state AND observed_generation = @observed_generation
  AND (health_code IS NULL OR health_code = ANY(@owned_codes::text[]));

-- name: MarkStaleManagedHealth :execrows
-- Health becomes unknown when the agent missed two syncs.
UPDATE app_instances SET health = 'unknown', health_code = 'agent_unavailable', updated_at = now()
WHERE management = 'managed' AND state IN ('active', 'degraded') AND health <> 'unknown'
  AND (health_checked_at IS NULL OR health_checked_at < @before::timestamptz)
  AND (health_code IS NULL OR health_code = ANY(@owned_codes::text[]));

-- Bindings -----------------------------------------------------------------

-- name: ListAppBindings :many
SELECT b.*, i.key AS instance_key, i.display_name AS instance_display_name, i.provider AS instance_provider, i.state AS instance_state
FROM app_bindings b JOIN app_instances i ON i.id = b.instance_id
ORDER BY b.capability, b.created_at, b.instance_id;

-- name: ListAppInstanceBindings :many
SELECT * FROM app_bindings WHERE instance_id = @instance_id ORDER BY capability;

-- name: InsertAppBinding :execrows
INSERT INTO app_bindings (capability, instance_id, is_default)
VALUES (@capability, @instance_id,
        NOT EXISTS (SELECT 1 FROM app_bindings d WHERE d.capability = @capability AND d.is_default))
ON CONFLICT (capability, instance_id) DO NOTHING;

-- name: ClearDefaultAppBinding :exec
UPDATE app_bindings SET is_default = false WHERE capability = @capability AND is_default;

-- name: SetDefaultAppBinding :execrows
UPDATE app_bindings SET is_default = true WHERE capability = @capability AND instance_id = @instance_id;

-- Credentials --------------------------------------------------------------

-- name: InsertAppCredential :one
INSERT INTO app_credentials (instance_id, name, version, ciphertext, state, activated_at)
VALUES (@instance_id, @name,
        (SELECT coalesce(max(c.version), 0) + 1 FROM app_credentials c WHERE c.instance_id = @instance_id AND c.name = @name),
        @ciphertext, @state, CASE WHEN @state::text = 'active' THEN now() END)
RETURNING *;

-- name: NextAppCredentialVersion :one
SELECT (coalesce(max(version), 0) + 1)::integer FROM app_credentials WHERE instance_id = @instance_id AND name = @name;

-- name: InsertAppCredentialVersion :one
INSERT INTO app_credentials (instance_id, name, version, ciphertext, state, activated_at)
VALUES (@instance_id, @name, @version, @ciphertext, @state, CASE WHEN @state::text = 'active' THEN now() END)
RETURNING *;

-- name: GetAppCredential :one
SELECT * FROM app_credentials WHERE instance_id = @instance_id AND name = @name AND version = @version;

-- name: GetAppCredentialForUpdate :one
SELECT * FROM app_credentials WHERE instance_id = @instance_id AND name = @name AND version = @version FOR UPDATE;

-- name: GetAppCredentialCandidateForUpdate :one
SELECT * FROM app_credentials
WHERE instance_id = @instance_id AND name = @name AND state IN ('pending', 'uncertain', 'abandoning')
FOR UPDATE;

-- name: GetActiveAppCredential :one
SELECT * FROM app_credentials WHERE instance_id = @instance_id AND name = @name AND state = 'active';

-- name: GetAppCredentialCandidate :one
-- The version being rotated in, if any (abandoning included).
SELECT * FROM app_credentials WHERE instance_id = @instance_id AND name = @name AND state IN ('pending', 'uncertain', 'abandoning');

-- name: ListUsableAppCredentials :many
-- Pending, uncertain, active and not yet retired versions, newest first:
-- what incoming verification accepts during a changeover.
SELECT * FROM app_credentials
WHERE instance_id = @instance_id AND name = @name
  AND (state IN ('pending', 'uncertain', 'abandoning', 'active') OR (state = 'retiring' AND retire_at > now()))
ORDER BY version DESC;

-- name: ListAppCredentials :many
SELECT instance_id, name, version, state, created_at, activated_at, retire_at
FROM app_credentials WHERE instance_id = @instance_id AND state <> 'discarded'
  AND NOT (state = 'retiring' AND retire_at <= now())
ORDER BY name, version DESC;

-- name: SetAppCredentialStateFrom :execrows
-- A discarded version ends now (retention counts from then).
UPDATE app_credentials
SET state = @state, ended_at = CASE WHEN @state::text = 'discarded' THEN now() ELSE ended_at END
WHERE id = @id AND state = ANY(@from_states::text[]);

-- name: SetAppCredentialCiphertext :exec
UPDATE app_credentials SET ciphertext = @ciphertext WHERE id = @id;

-- name: RetireActiveAppCredential :exec
UPDATE app_credentials SET state = 'retiring', retire_at = @retire_at, ended_at = now()
WHERE instance_id = @instance_id AND name = @name AND state = 'active';

-- name: AppCredentialCallInFlight :one
-- Whether an execution of this credential (any row) may still be acted on
-- by Orglane: its lease has not expired, by database time.
SELECT EXISTS (
    SELECT 1 FROM app_credential_calls k JOIN app_credentials c ON c.id = k.credential_id
    WHERE c.instance_id = @instance_id AND c.name = @name AND k.deadline > now()
);

-- name: InsertAppCredentialCall :exec
-- Starts an execution writing the row's value; its lease never shrinks.
INSERT INTO app_credential_calls (id, credential_id, deadline)
VALUES (@id, @credential_id, now() + make_interval(secs => @lease_seconds::double precision));

-- name: EndAppCredentialCall :execrows
-- Ends an execution after a definite answer, only while its own lease holds:
-- zero rows means it may no longer record its result.
DELETE FROM app_credential_calls WHERE id = @id AND deadline > now();

-- name: DeleteExpiredAppCredentialCalls :execrows
DELETE FROM app_credential_calls WHERE deadline < now() - interval '1 day';

-- name: ListRetainedAppCredentials :many
-- Retired and discarded versions still retained: incoming signatures are
-- checked against them to detect drift, never accepted with them.
SELECT * FROM app_credentials
WHERE instance_id = @instance_id AND name = @name
  AND (state = 'discarded' OR (state = 'retiring' AND retire_at <= now()))
ORDER BY version DESC;

-- name: ListDriftAppCredentials :many
-- Usable versions that are neither active nor a candidate (retiring within
-- their overlap), newest first.
SELECT * FROM app_credentials
WHERE instance_id = @instance_id AND name = @name AND state = 'retiring' AND retire_at > now()
ORDER BY version DESC;

-- name: MarkAppCredentialDrift :execrows
-- One drift report per version, and only once the version ended longer ago
-- than the grace period (deliveries signed just before a change).
UPDATE app_credentials SET drift_detected_at = now()
WHERE id = @id AND state IN ('retiring', 'discarded') AND drift_detected_at IS NULL
  AND ended_at < now() - interval '5 minutes';

-- name: ListAppCredentialDrift :many
-- Older versions a delivery was signed with since the current active version
-- became active.
SELECT c.name, c.version, c.drift_detected_at::timestamptz AS detected_at
FROM app_credentials c
JOIN app_credentials a ON a.instance_id = c.instance_id AND a.name = c.name AND a.state = 'active'
WHERE c.instance_id = @instance_id AND c.drift_detected_at IS NOT NULL AND c.drift_detected_at > a.activated_at
ORDER BY c.name, c.version DESC;

-- name: ActivateAppCredential :execrows
UPDATE app_credentials SET state = 'active', activated_at = now(), retire_at = NULL
WHERE id = @id AND state IN ('pending', 'uncertain', 'abandoning');

-- name: ListStuckAppCredentials :many
SELECT * FROM app_credentials
WHERE state IN ('uncertain', 'abandoning') AND created_at < @before AND stuck_reported_at IS NULL;

-- name: MarkAppCredentialStuckReported :execrows
-- One rotation_stuck report per credential row.
UPDATE app_credentials SET stuck_reported_at = now()
WHERE id = @id AND state IN ('uncertain', 'abandoning') AND stuck_reported_at IS NULL;

-- name: DeleteExpiredAppCredentials :execrows
-- Older versions are kept 7 days after they stopped being accepted (retired:
-- past their overlap; discarded: from the discard) to detect drift; they are
-- never accepted meanwhile.
DELETE FROM app_credentials
WHERE (state = 'retiring' AND retire_at <= now() - interval '7 days')
   OR (state = 'discarded' AND ended_at < now() - interval '7 days');

-- Account links ------------------------------------------------------------

-- name: GetVerifiedAccountLink :one
SELECT * FROM account_links
WHERE person_id = @person_id AND provider = @provider AND realm = @realm AND state = 'verified';

-- name: GetVerifiedAccountLinkByAccount :one
SELECT * FROM account_links
WHERE provider = @provider AND realm = @realm AND external_id = @external_id AND state = 'verified';

-- name: InsertAccountLink :one
INSERT INTO account_links (person_id, provider, realm, external_id, login, method, instance_id, evidence)
VALUES (@person_id, @provider, @realm, @external_id, @login, @method, @instance_id, @evidence)
RETURNING *;

-- OIDC access of app clients (apps.Service.OIDCAccessTx): read under share
-- locks in the transaction that issues a code or tokens, so a withdrawal
-- (an UPDATE of any of these rows) is ordered before or after it, never
-- in between.

-- name: GetAppInstanceByKeyForShare :one
SELECT * FROM app_instances WHERE key = @key AND (state <> 'removed' OR data_retained) FOR SHARE;

-- name: GetVerifiedAccountLinkForShare :one
SELECT * FROM account_links
WHERE person_id = @person_id AND provider = @provider AND realm = @realm AND state = 'verified'
FOR SHARE;

-- name: GetProvisioningResultForShare :one
SELECT * FROM provisioning_results WHERE instance_id = @instance_id AND person_id = @person_id FOR SHARE;

-- name: RefreshAccountLinkLogin :execrows
-- The provider-side login of a verified link changed (informational: links
-- are keyed by the immutable external ID). Only that person's link of that
-- account changes.
UPDATE account_links SET login = @login, updated_at = now()
WHERE person_id = @person_id AND provider = @provider AND realm = @realm AND external_id = @external_id
  AND state = 'verified' AND login <> @login;

-- name: RevokeAccountLink :one
UPDATE account_links SET state = 'revoked', revoked_at = now(), updated_at = now()
WHERE id = @id AND state = 'verified'
RETURNING *;

-- name: ListPersonAccountLinks :many
SELECT * FROM account_links WHERE person_id = @person_id AND state = 'verified' ORDER BY provider, realm;

-- name: ListRealmAccountLinks :many
SELECT * FROM account_links WHERE provider = @provider AND realm = @realm AND state = 'verified';

-- name: ListInstancePeopleWithLinks :many
SELECT DISTINCT person_id FROM account_links
WHERE state = 'verified' AND provider = @provider AND realm = @realm;

-- OAuth state --------------------------------------------------------------

-- name: InsertAppOAuthState :one
INSERT INTO app_oauth_states (id, kind, state_hash, instance_id, person_id, session_id, step, pkce_verifier, expires_at)
VALUES (@id, @kind, @state_hash, @instance_id, @person_id, @session_id, @step, @pkce_verifier, @expires_at)
RETURNING *;

-- name: ConsumeAppOAuthState :one
-- Atomic and single-use: one answer for expired, used, foreign and unknown
-- states (no row).
UPDATE app_oauth_states SET consumed_at = now()
WHERE state_hash = @state_hash AND kind = @kind AND consumed_at IS NULL AND expires_at > now()
  AND session_id = @session_id AND person_id = @person_id
RETURNING *;

-- name: DeleteExpiredAppOAuthStates :execrows
DELETE FROM app_oauth_states WHERE expires_at < now() - interval '24 hours';

-- Team mappings ------------------------------------------------------------

-- name: ListAppTeamMappings :many
SELECT m.*, t.slug AS team_slug, t.name AS team_name, t.archived_at AS team_archived_at
FROM app_team_mappings m JOIN teams t ON t.id = m.team_id
WHERE m.instance_id = @instance_id
ORDER BY t.name, t.id;

-- name: GetAppTeamMapping :one
SELECT * FROM app_team_mappings WHERE instance_id = @instance_id AND team_id = @team_id;

-- name: UpsertAppTeamMapping :one
INSERT INTO app_team_mappings (instance_id, team_id, mode, external_ref, external_name)
VALUES (@instance_id, @team_id, @mode, @external_ref, @external_name)
ON CONFLICT (instance_id, team_id) DO UPDATE
SET mode = EXCLUDED.mode, external_ref = EXCLUDED.external_ref, external_name = EXCLUDED.external_name, updated_at = now()
RETURNING *;

-- name: DeleteAppTeamMapping :execrows
DELETE FROM app_team_mappings WHERE instance_id = @instance_id AND team_id = @team_id;

-- name: SetAppTeamMappingExternal :exec
-- Records the provider team a sync found or created for a mapping, unless
-- the mapping changed since the sync read it (mode or reference).
UPDATE app_team_mappings SET external_ref = @external_ref, external_name = @external_name, updated_at = now()
WHERE instance_id = @instance_id AND team_id = @team_id
  AND mode = @mode AND external_ref IS NOT DISTINCT FROM sqlc.narg(synced_ref);

-- name: ListPersonMappedTeams :many
-- The provider teams of a person's direct memberships in non-archived teams.
-- Mappings whose provider team does not exist yet grant nothing.
SELECT m.team_id, m.external_ref, coalesce(m.external_name, t.slug)::text AS external_name
FROM app_team_mappings m
JOIN team_memberships tm ON tm.team_id = m.team_id AND tm.person_id = @person_id
JOIN teams t ON t.id = m.team_id AND t.archived_at IS NULL
WHERE m.instance_id = @instance_id AND m.external_ref IS NOT NULL
ORDER BY m.team_id;

-- name: CountPersonMappings :one
-- Mappings of a person's non-archived teams, synced or not.
SELECT count(*) FROM app_team_mappings m
JOIN team_memberships tm ON tm.team_id = m.team_id AND tm.person_id = @person_id
JOIN teams t ON t.id = m.team_id AND t.archived_at IS NULL
WHERE m.instance_id = @instance_id;

-- name: ListTeamMemberIDs :many
SELECT person_id FROM team_memberships WHERE team_id = @team_id ORDER BY person_id;

-- People -------------------------------------------------------------------

-- name: ListProvisioningCandidates :many
-- Everyone provision_instance looks at: active people, and people with a
-- result row or a link at the instance.
SELECT p.id FROM people p
WHERE p.status = 'active'
   OR EXISTS (SELECT 1 FROM provisioning_results r WHERE r.instance_id = @instance_id AND r.person_id = p.id)
   OR EXISTS (SELECT 1 FROM account_links l WHERE l.state = 'verified' AND l.provider = @provider AND l.realm = @realm AND l.person_id = p.id)
ORDER BY p.id;

-- Provisioning results -----------------------------------------------------

-- name: GetProvisioningResult :one
SELECT * FROM provisioning_results WHERE instance_id = @instance_id AND person_id = @person_id;

-- name: MarkProvisioningQueued :exec
-- A failed result stays failed (with its error) until a run decides: only
-- a desired-state change or an admin's retry runs it again.
UPDATE provisioning_results
SET status = CASE WHEN status = 'failed' THEN status ELSE 'queued' END,
    employee_step = NULL, next_attempt_at = clock_timestamp(), updated_at = now()
WHERE instance_id = @instance_id AND person_id = @person_id;

-- name: MarkProvisioningGranted :exec
-- Orglane granted access (created the account, the invitation or the
-- membership): recorded with the action's audit entry, never undone. It
-- resolves the pending grant.
INSERT INTO provisioning_results (instance_id, person_id, status, access, granted_by_orglane)
VALUES (@instance_id, @person_id, 'queued', true, true)
ON CONFLICT (instance_id, person_id) DO UPDATE SET granted_by_orglane = true, pending_grant = NULL, updated_at = now();

-- name: SetPendingGrant :exec
-- Durable intent, committed before a granting action reaches the provider.
INSERT INTO provisioning_results (instance_id, person_id, status, access, pending_grant)
VALUES (@instance_id, @person_id, 'queued', true, @pending_grant)
ON CONFLICT (instance_id, person_id) DO UPDATE SET pending_grant = EXCLUDED.pending_grant, updated_at = now();

-- name: ClearProvisioningOwnership :exec
-- A person's verified link was revoked or replaced: the access Orglane
-- granted to that account is no longer the person's (the account becomes
-- unmanaged: reported, never removed), and no interrupted grant for it is
-- replayed.
UPDATE provisioning_results SET granted_by_orglane = false, pending_grant = NULL, updated_at = now()
WHERE person_id = @person_id AND instance_id = ANY(@instance_ids::uuid[])
  AND (granted_by_orglane OR pending_grant IS NOT NULL);

-- name: ClearPendingGrant :exec
-- The provider definitely rejected the grant or the access already existed:
-- the intent is resolved without ownership.
UPDATE provisioning_results SET pending_grant = NULL, updated_at = now()
WHERE instance_id = @instance_id AND person_id = @person_id;

-- name: FailProvisioningResult :exec
-- A removal failed permanently (or exhausted its retries): the person's
-- result shows it until desired access changes or an admin retries.
UPDATE provisioning_results
SET status = 'failed', employee_step = NULL, error_code = @error_code, actions = @actions,
    desired_hash = @desired_hash, attempts = attempts + 1, last_attempt_at = now(), updated_at = now()
WHERE instance_id = @instance_id AND person_id = @person_id;

-- name: UpsertProvisioningResult :one
INSERT INTO provisioning_results (instance_id, person_id, status, access, employee_step, granted_by_orglane,
    desired_hash, applied_hash, actions, error_code, attempts, last_attempt_at, next_attempt_at, updated_at)
VALUES (@instance_id, @person_id, @status, @access, @employee_step, @granted_by_orglane,
    @desired_hash, @applied_hash, @actions, @error_code, @attempts, now(), @next_attempt_at, now())
ON CONFLICT (instance_id, person_id) DO UPDATE
SET status = EXCLUDED.status, access = EXCLUDED.access, employee_step = EXCLUDED.employee_step,
    granted_by_orglane = provisioning_results.granted_by_orglane OR EXCLUDED.granted_by_orglane,
    desired_hash = EXCLUDED.desired_hash,
    applied_hash = EXCLUDED.applied_hash, actions = EXCLUDED.actions, error_code = EXCLUDED.error_code,
    attempts = EXCLUDED.attempts, last_attempt_at = now(),
    -- A change queued while this run worked keeps its marker (it is newer
    -- than the run's start), so the run knows to go again.
    next_attempt_at = CASE WHEN provisioning_results.next_attempt_at > @started_at THEN provisioning_results.next_attempt_at
                           ELSE EXCLUDED.next_attempt_at END,
    updated_at = now()
RETURNING *;

-- name: ListProvisioningResults :many
SELECT r.*, p.username, p.display_name, p.status AS person_status
FROM provisioning_results r JOIN people p ON p.id = r.person_id
WHERE r.instance_id = @instance_id
ORDER BY p.display_name, p.id
LIMIT @row_limit OFFSET @row_offset;

-- name: CountProvisioningResults :many
SELECT status, count(*) AS n FROM provisioning_results WHERE instance_id = @instance_id GROUP BY status ORDER BY status;

-- name: ListPersonProvisioningResults :many
SELECT r.*, i.key AS instance_key, i.display_name AS instance_display_name, i.provider AS instance_provider, i.state AS instance_state
FROM provisioning_results r JOIN app_instances i ON i.id = r.instance_id
WHERE r.person_id = @person_id
ORDER BY i.display_name, i.id;

-- name: ListFailedProvisioningResults :many
SELECT person_id FROM provisioning_results
WHERE instance_id = @instance_id AND status = 'failed'
  AND (sqlc.narg(person_id)::uuid IS NULL OR person_id = sqlc.narg(person_id)::uuid)
ORDER BY person_id;

-- name: CountManagedAccounts :one
-- Accounts the instance manages: Orglane granted them, or the instance
-- manages existing members and the person was provisioned.
SELECT count(*) FROM provisioning_results r JOIN app_instances i ON i.id = r.instance_id
WHERE r.instance_id = @instance_id AND r.applied_hash IS NOT NULL
  AND (r.granted_by_orglane OR i.manage_existing_members);

-- Removal guard ------------------------------------------------------------

-- name: UpsertPendingRemoval :exec
-- Held removals stay as the admin saw them; a changed plan for them is
-- caught by the freshness check when they are confirmed.
INSERT INTO provisioning_removals (instance_id, person_id, action, action_key, desired_hash, state)
VALUES (@instance_id, @person_id, @action, @action_key, @desired_hash, 'pending')
ON CONFLICT (instance_id, person_id, action_key) WHERE state IN ('pending', 'held') DO UPDATE
SET action = EXCLUDED.action, desired_hash = EXCLUDED.desired_hash, updated_at = now()
WHERE provisioning_removals.state = 'pending';

-- name: DropPendingRemovals :execrows
-- Pending removals of a person that the current plan no longer contains. An
-- accepted removal (error_code = accepted_code) is kept: the provider may
-- already have acted, so it must be confirmed and audited instead of dropped.
UPDATE provisioning_removals SET state = 'dropped', updated_at = now()
WHERE instance_id = @instance_id AND person_id = @person_id AND state = 'pending'
  AND NOT (action_key = ANY(@keep_keys::text[]))
  AND (error_code IS NULL OR error_code <> sqlc.arg(accepted_code)::text);

-- name: ListPendingRemovals :many
SELECT * FROM provisioning_removals WHERE instance_id = @instance_id AND state = 'pending'
ORDER BY created_at, id FOR UPDATE;

-- name: ListBatchRemovals :many
SELECT * FROM provisioning_removals WHERE batch_id = @batch_id ORDER BY created_at, id;

-- name: ListHeldRemovals :many
SELECT r.*, p.username, p.display_name, p.status AS person_status
FROM provisioning_removals r JOIN people p ON p.id = r.person_id
WHERE r.batch_id = @batch_id AND r.state = 'held'
ORDER BY r.created_at, r.id;

-- name: CountAppliedRemovalsSince :one
SELECT count(*) FROM provisioning_removals WHERE instance_id = @instance_id AND state = 'applied' AND updated_at > @since;

-- name: CountHeldBatchRemovals :one
SELECT count(*) FROM provisioning_removals WHERE batch_id = @batch_id AND state = 'held';

-- name: DropOpenRemovals :exec
-- After a person's removal failed, their later removals do not run. An
-- accepted removal is not a later removal: keep it so its completion is still
-- confirmed and audited.
UPDATE provisioning_removals SET state = 'dropped', updated_at = now()
WHERE instance_id = @instance_id AND person_id = @person_id AND state IN ('pending', 'held')
  AND (error_code IS NULL OR error_code <> sqlc.arg(accepted_code)::text);

-- name: GetRemoval :one
SELECT * FROM provisioning_removals WHERE id = @id;

-- name: SetRemovalStateFrom :execrows
UPDATE provisioning_removals SET state = @state, error_code = @error_code, updated_at = now()
WHERE id = @id AND state = @from_state;

-- name: SetRemovalState :exec
UPDATE provisioning_removals SET state = @state, error_code = @error_code, updated_at = now() WHERE id = @id;

-- name: HoldRemovals :exec
UPDATE provisioning_removals SET state = 'held', batch_id = @batch_id, updated_at = now() WHERE id = ANY(@ids::uuid[]);

-- name: InsertRemovalBatch :one
INSERT INTO provisioning_removal_batches (instance_id, state, item_count, items_hash)
VALUES (@instance_id, 'held', @item_count, @items_hash)
RETURNING *;

-- name: GetHeldRemovalBatch :one
SELECT * FROM provisioning_removal_batches WHERE instance_id = @instance_id AND state = 'held' FOR UPDATE;

-- name: GetRemovalBatchForUpdate :one
SELECT * FROM provisioning_removal_batches WHERE id = @id AND instance_id = @instance_id FOR UPDATE;

-- name: ListConfirmedRemovalBatches :many
SELECT * FROM provisioning_removal_batches WHERE instance_id = @instance_id AND state = 'confirmed' ORDER BY created_at FOR UPDATE;

-- name: SetRemovalBatchState :exec
UPDATE provisioning_removal_batches SET state = @state WHERE id = @id;

-- name: ConfirmRemovalBatch :exec
UPDATE provisioning_removal_batches SET state = 'confirmed', confirmed_by = @confirmed_by, confirmed_at = now() WHERE id = @id;

-- Unmanaged accounts -------------------------------------------------------

-- name: DeleteUnmanagedAccounts :exec
DELETE FROM app_unmanaged_accounts WHERE instance_id = @instance_id;

-- name: InsertUnmanagedAccount :exec
INSERT INTO app_unmanaged_accounts (instance_id, external_id, login) VALUES (@instance_id, @external_id, @login)
ON CONFLICT (instance_id, external_id) DO UPDATE SET login = EXCLUDED.login, seen_at = now();

-- name: ListUnmanagedAccounts :many
SELECT * FROM app_unmanaged_accounts WHERE instance_id = @instance_id ORDER BY login, external_id;

-- name: CountUnmanagedAccounts :one
SELECT count(*) FROM app_unmanaged_accounts WHERE instance_id = @instance_id;

-- Webhook deliveries and events --------------------------------------------

-- name: InsertWebhookDelivery :execrows
-- Conflicts on either unique key (delivery ID or signed body) are
-- duplicates.
INSERT INTO webhook_deliveries (id, instance_id, delivery_id, event_name, received_at, body, headers, body_sha256)
VALUES (@id, @instance_id, @delivery_id, @event_name, @received_at, @body, @headers, @body_sha256)
ON CONFLICT DO NOTHING;

-- name: GetWebhookDeliveryForUpdate :one
SELECT * FROM webhook_deliveries WHERE id = @id FOR UPDATE;

-- name: FinishWebhookDelivery :exec
UPDATE webhook_deliveries SET status = @status, error_code = @error_code, event_count = @event_count, processed_at = now()
WHERE id = @id;

-- name: ClearWebhookDeliveryBodies :execrows
UPDATE webhook_deliveries SET body = NULL
WHERE body IS NOT NULL AND (
      (status IN ('processed', 'ignored') AND processed_at < now() - interval '7 days')
   OR (status = 'failed' AND processed_at < now() - interval '30 days'));

-- name: DeleteOldWebhookDeliveries :execrows
DELETE FROM webhook_deliveries WHERE received_at < now() - interval '30 days';

-- name: InsertWebhookEffectMark :execrows
-- Records that a signed body was considered for delivery effects. A replay
-- after delivery retention finds the mark and applies no effect. Marks live
-- as long as their instance and are never deleted by retention.
INSERT INTO webhook_effect_marks (instance_id, body_sha256, effect, code, considered_at)
VALUES (@instance_id, @body_sha256, @effect, @code, @considered_at)
ON CONFLICT (instance_id, body_sha256) DO NOTHING;

-- name: InsertEvent :execrows
INSERT INTO events (type, schema_version, instance_id, provider, delivery_id, dedupe_key, occurred_at, received_at, actor, subject, targets, data)
VALUES (@type, @schema_version, @instance_id, @provider, @delivery_id, @dedupe_key, @occurred_at, @received_at, @actor, @subject, @targets, @data)
ON CONFLICT (instance_id, dedupe_key) DO NOTHING;

-- name: ListPersonPendingAccountLinks :many
-- Import evidence (migration 00012): never a proof of ownership.
SELECT * FROM pending_account_links WHERE person_id = @person_id ORDER BY provider, organization_id, login;

-- name: ListInstanceEvents :many
SELECT * FROM events WHERE instance_id = @instance_id ORDER BY occurred_at DESC, id DESC LIMIT @row_limit;

-- name: GetAppConnectStateInstance :one
-- Lookup before taking the per-instance connect lock; consumption must still
-- recheck all predicates under that lock, so a resume cannot revive stale state.
SELECT instance_id FROM app_oauth_states
WHERE state_hash = @state_hash AND kind = 'connect' AND person_id = @person_id
  AND session_id = @session_id AND consumed_at IS NULL AND expires_at > now();

-- name: InvalidateAppConnectStates :exec
UPDATE app_oauth_states SET consumed_at = now()
WHERE instance_id = @instance_id AND kind = 'connect' AND consumed_at IS NULL;

-- name: SetConnectedAppBaseURL :exec
UPDATE app_instances SET base_url = @base_url, updated_at = now()
WHERE id = @id AND state = 'connecting' AND management = 'connected';

-- name: ListAppCredentialRotations :many
-- Live candidates are authoritative, even while an earlier outcome is retained.
-- History survives ciphertext cleanup and version reuse. UUIDv7 IDs are generated
-- on INSERT, after the credential mutation/row locks; occurred_at is transaction
-- start time and can invert a waiting acceptance and the preceding completion.
-- Read only safe metadata, never ciphertext.
WITH candidates AS (
    SELECT name, version,
        (CASE state WHEN 'pending' THEN 'accepted' WHEN 'uncertain' THEN 'verifying'
                    ELSE 'cancelling' END)::text AS state
    FROM app_credentials
    WHERE instance_id = @instance_id AND state IN ('pending', 'uncertain', 'abandoning')
), latest AS (
    SELECT DISTINCT ON (metadata->>'name') metadata->>'name' AS name,
        (metadata->>'version')::integer AS version, action, metadata->>'stage' AS stage,
        metadata->>'resolution' AS resolution
    FROM audit_entries
    WHERE target_kind = 'app_instance' AND target_id = @target_id
      AND action IN ('app_credential.rotation_started', 'app_credential.rotation_failed',
          'app_credential.rotated', 'app_credential.rotation_abandoned',
          'app_credential.rotation_resolved')
    ORDER BY metadata->>'name', id DESC
)
SELECT name, version, state FROM candidates
UNION ALL
SELECT latest.name::text AS name, latest.version,
    (CASE WHEN latest.action = 'app_credential.rotation_failed' THEN 'failed'
          WHEN latest.action = 'app_credential.rotated' THEN 'verified'
          WHEN latest.action = 'app_credential.rotation_abandoned' THEN
              CASE WHEN latest.stage = 'discarded' THEN 'cancelled' ELSE 'cancelling' END
          WHEN latest.action = 'app_credential.rotation_resolved' THEN
              CASE WHEN latest.resolution = 'active' THEN 'activated_by_admin' ELSE 'discarded_by_admin' END
          ELSE 'accepted' END)::text AS state
FROM latest
WHERE NOT EXISTS (SELECT 1 FROM candidates WHERE candidates.name = latest.name)
ORDER BY name;

-- Account linking (docs/specs/providers.md "Account links") -----------------

-- name: InvalidateAppLinkStates :exec
-- A new start invalidates the person's older link states for the instance.
UPDATE app_oauth_states SET consumed_at = now()
WHERE kind = 'link' AND person_id = @person_id AND instance_id = @instance_id AND consumed_at IS NULL;

-- name: GetAccountLink :one
SELECT * FROM account_links WHERE id = @id;

-- name: ListPendingAccountLinksForOrganization :many
SELECT * FROM pending_account_links
WHERE provider = @provider AND organization_id = @organization_id
ORDER BY person_id, external_id;

-- name: ListPersonPendingAccountLinksWithConflict :many
-- A person's import evidence, and whether the account it names is verified
-- for someone else (shown as conflicting, never resolved automatically).
SELECT p.*, EXISTS (
    SELECT 1 FROM account_links l
    WHERE l.state = 'verified' AND l.provider = p.provider AND l.realm = 'github.com'
      AND l.external_id = p.external_id::text AND l.person_id <> p.person_id
)::boolean AS conflict
FROM pending_account_links p WHERE p.person_id = @person_id
ORDER BY p.provider, p.organization_id, p.login;

-- Install, removal and reinstall of managed instances (#61) ----------------

-- name: ListReservedAppInstanceHosts :many
-- Base URLs of the instances that hold their key (and so their host): not
-- removed, or removed with their data retained.
SELECT id, base_url::text AS base_url FROM app_instances
WHERE (state <> 'removed' OR data_retained) AND base_url IS NOT NULL;

-- name: DeleteAppInstanceBindings :execrows
DELETE FROM app_bindings WHERE instance_id = @instance_id;

-- name: DeleteAppInstanceCredentials :execrows
-- A purge deletes every credential version of the instance.
DELETE FROM app_credentials WHERE instance_id = @instance_id;

-- name: SetAppInstanceRemoved :execrows
-- The removal's operation succeeded. Conditional on the state and on the
-- generation that removed it, so a stale result changes nothing.
UPDATE app_instances
SET state = 'removed', data_retained = @data_retained, removed_at = now(),
    health = 'unknown', health_code = NULL, health_checked_at = now(), updated_at = now()
WHERE id = @id AND state = 'removing' AND desired_generation = @generation;

-- name: SetAppInstanceReinstalling :execrows
-- A removed instance with retained data comes back under the same ID.
UPDATE app_instances
SET state = 'installing', data_retained = false, removed_at = NULL,
    health = 'unknown', health_code = NULL, health_checked_at = now(), updated_at = now()
WHERE id = @id AND state = 'removed' AND data_retained;

-- name: ClearAppInstanceRetained :execrows
-- A purge of a retained instance: from here its data counts as being
-- deleted (a failed purge leaves it failed; removing it again retains it).
UPDATE app_instances SET data_retained = false, updated_at = now()
WHERE id = @id AND state = 'removed' AND data_retained;

-- name: CountAcceptedRemovals :one
-- A person's open removals the provider accepted but has not confirmed
-- (error_code = accepted_code): pending ones, and held ones released by a
-- confirmed batch (their row stays held until settled). While one is open,
-- the person's grants wait: the accepted removal may still land on access a
-- grant would restore.
SELECT count(*) FROM provisioning_removals
WHERE instance_id = @instance_id AND person_id = @person_id AND state IN ('pending', 'held')
  AND error_code = sqlc.arg(accepted_code)::text;

-- name: MarkProvisioningResultWaiting :exec
-- A queued result shows why it waits (the provider accepted a removal and
-- Orglane polls for its confirmation).
UPDATE provisioning_results SET error_code = @error_code, actions = @actions, updated_at = now()
WHERE instance_id = @instance_id AND person_id = @person_id AND status = 'queued';

-- name: RecordServedRoutes :exec
-- Replaces the served routes with those of a configuration just served:
-- keys no longer served are dropped, new or changed routes start their
-- served_since, unchanged ones keep it (and are not written).
WITH dropped AS (
    DELETE FROM ingress_served_routes
    WHERE instance_key <> ALL(@keys::text[])
)
INSERT INTO ingress_served_routes AS r (instance_key, host, upstream, maintenance, served_since)
SELECT unnest(@keys::text[]), unnest(@hosts::text[]), unnest(@upstreams::text[]),
    unnest(@maintenance::boolean[]), clock_timestamp()
ON CONFLICT (instance_key) DO UPDATE
SET host = excluded.host, upstream = excluded.upstream, maintenance = excluded.maintenance,
    served_since = excluded.served_since
WHERE (r.host, r.upstream, r.maintenance) IS DISTINCT FROM (excluded.host, excluded.upstream, excluded.maintenance);

-- name: GetServedRoute :one
-- The served route of an instance key, and whether it has been served
-- unchanged for at least settle_seconds (on the database's clock, shared
-- by every core process).
SELECT host, upstream, maintenance,
    (served_since <= clock_timestamp() - make_interval(secs => @settle_seconds::float8))::boolean AS settled
FROM ingress_served_routes
WHERE instance_key = @instance_key;
