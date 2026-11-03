-- name: CreatePerson :one
INSERT INTO people (username, work_email, display_name, status)
VALUES (@username, @work_email, @display_name, @status)
RETURNING *;

-- name: GetPerson :one
SELECT * FROM people WHERE id = @id;

-- name: GetPersonForUpdate :one
SELECT * FROM people WHERE id = @id FOR UPDATE;

-- name: GetPersonByWorkEmail :one
SELECT * FROM people WHERE lower(work_email) = lower(@work_email);

-- name: UpdatePerson :one
UPDATE people
SET username = @username, work_email = @work_email, display_name = @display_name, updated_at = now()
WHERE id = @id
RETURNING *;

-- name: SetPersonStatus :one
UPDATE people SET status = @status, updated_at = now()
WHERE id = @id
RETURNING *;
