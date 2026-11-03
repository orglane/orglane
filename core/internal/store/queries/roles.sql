-- name: InsertRoleIfMissing :one
-- Returns no row when a role with the key already exists.
INSERT INTO roles (key, name, builtin)
VALUES (@key, @name, @builtin)
ON CONFLICT (key) DO NOTHING
RETURNING *;

-- name: GetRoleByKey :one
SELECT * FROM roles WHERE key = @key;

-- name: GetRoleByKeyForUpdate :one
SELECT * FROM roles WHERE key = @key FOR UPDATE;

-- name: ListRoles :many
SELECT * FROM roles ORDER BY id;

-- name: UpdateRole :one
UPDATE roles SET name = @name, builtin = @builtin, updated_at = now()
WHERE id = @id
RETURNING *;

-- name: ListRolePermissions :many
SELECT permission FROM role_permissions WHERE role_id = @role_id ORDER BY permission;

-- name: AddRolePermission :exec
INSERT INTO role_permissions (role_id, permission) VALUES (@role_id, @permission);

-- name: RemoveRolePermission :exec
DELETE FROM role_permissions WHERE role_id = @role_id AND permission = @permission;

-- name: AssignPersonRole :execrows
INSERT INTO person_roles (person_id, role_id)
VALUES (@person_id, @role_id)
ON CONFLICT DO NOTHING;

-- name: UnassignPersonRole :execrows
DELETE FROM person_roles WHERE person_id = @person_id AND role_id = @role_id;

-- name: ListPersonRoles :many
SELECT r.*
FROM roles r
JOIN person_roles pr ON pr.role_id = r.id
WHERE pr.person_id = @person_id
ORDER BY r.id;

-- name: ListPersonPermissions :many
SELECT DISTINCT rp.permission
FROM person_roles pr
JOIN role_permissions rp ON rp.role_id = pr.role_id
WHERE pr.person_id = @person_id
ORDER BY rp.permission;

-- name: CountActiveHoldersExcept :one
-- Counts active people holding the role, other than the given person.
SELECT count(*)
FROM person_roles pr
JOIN people p ON p.id = pr.person_id
WHERE pr.role_id = @role_id AND p.status = 'active' AND p.id <> @person_id;

-- name: PersonHasRole :one
SELECT EXISTS (SELECT 1 FROM person_roles WHERE person_id = @person_id AND role_id = @role_id);
