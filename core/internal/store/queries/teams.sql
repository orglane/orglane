-- name: CreateTeam :one
INSERT INTO teams (slug, name, description, parent_id)
VALUES (@slug, @name, sqlc.narg(description), sqlc.narg(parent_id))
RETURNING *;

-- name: GetTeamForUpdate :one
SELECT * FROM teams WHERE id = @id FOR UPDATE;

-- name: GetTeamBySlug :one
SELECT * FROM teams WHERE slug = @slug;

-- name: UpdateTeam :one
UPDATE teams
SET slug = @slug, name = @name, description = sqlc.narg(description), parent_id = sqlc.narg(parent_id), updated_at = now()
WHERE id = @id
RETURNING *;

-- name: ArchiveTeam :one
UPDATE teams SET archived_at = now(), updated_at = now()
WHERE id = @id
RETURNING *;

-- name: GetTeamWithMemberCount :one
-- member_count counts members who are not offboarded, like the directory.
SELECT
    sqlc.embed(t),
    (SELECT count(*) FROM team_memberships m JOIN people p ON p.id = m.person_id
     WHERE m.team_id = t.id AND p.status <> 'offboarded') AS member_count
FROM teams t
WHERE t.id = @id;

-- name: ListTeams :many
-- Teams by name with their member counts; archived teams only when asked.
SELECT
    sqlc.embed(t),
    (SELECT count(*) FROM team_memberships m JOIN people p ON p.id = m.person_id
     WHERE m.team_id = t.id AND p.status <> 'offboarded') AS member_count
FROM teams t
WHERE @include_archived::boolean OR t.archived_at IS NULL
ORDER BY lower(t.name), t.id;

-- name: SharesTeam :one
-- A shared archived team or an offboarded person never grants visibility.
SELECT EXISTS (
    SELECT 1
    FROM team_memberships a
    JOIN team_memberships b ON b.team_id = a.team_id
    JOIN teams t ON t.id = a.team_id
    JOIN people pa ON pa.id = a.person_id
    JOIN people pb ON pb.id = b.person_id
    WHERE a.person_id = @person_a AND b.person_id = @person_b
      AND t.archived_at IS NULL
      AND pa.status <> 'offboarded' AND pb.status <> 'offboarded'
) AS shares_team;

-- name: ListTeamAncestors :many
-- The team and every team above it. The hierarchy is acyclic (package
-- identity checks every parent change), so the recursion ends.
WITH RECURSIVE chain (id, parent_id) AS (
    SELECT teams.id, teams.parent_id FROM teams WHERE teams.id = @id
    UNION ALL
    SELECT t.id, t.parent_id FROM teams t JOIN chain c ON t.id = c.parent_id
)
SELECT chain.id FROM chain;

-- name: CountActiveChildTeams :one
SELECT count(*) FROM teams WHERE parent_id = @parent_id AND archived_at IS NULL;

-- name: GetTeamMembershipForUpdate :one
SELECT * FROM team_memberships WHERE team_id = @team_id AND person_id = @person_id FOR UPDATE;

-- name: InsertTeamMembership :one
INSERT INTO team_memberships (team_id, person_id, role)
VALUES (@team_id, @person_id, @role)
RETURNING *;

-- name: SetTeamMembershipRole :one
UPDATE team_memberships SET role = @role, updated_at = now()
WHERE team_id = @team_id AND person_id = @person_id
RETURNING *;

-- name: DeleteTeamMembership :execrows
DELETE FROM team_memberships WHERE team_id = @team_id AND person_id = @person_id;

-- name: ListTeamMembers :many
-- The members of a team who are not offboarded: leads first, then by
-- display name.
SELECT
    sqlc.embed(p),
    m.role,
    m.created_at AS joined_at,
    EXISTS (SELECT 1 FROM person_avatars a WHERE a.person_id = p.id) AS has_avatar
FROM team_memberships m
JOIN people p ON p.id = m.person_id
WHERE m.team_id = @team_id AND p.status <> 'offboarded'
ORDER BY m.role = 'lead' DESC, lower(p.display_name), p.id;

-- name: ListPersonTeams :many
-- The teams a person belongs to, archived teams excluded, by name.
SELECT
    sqlc.embed(t),
    m.role,
    (SELECT count(*) FROM team_memberships m2 JOIN people p ON p.id = m2.person_id
     WHERE m2.team_id = t.id AND p.status <> 'offboarded') AS member_count
FROM team_memberships m
JOIN teams t ON t.id = m.team_id
WHERE m.person_id = @person_id AND t.archived_at IS NULL
ORDER BY lower(t.name), t.id;
