-- name: PutPersonAvatar :exec
INSERT INTO person_avatars (person_id, content_type, data)
VALUES (@person_id, @content_type, @data)
ON CONFLICT (person_id) DO UPDATE
SET content_type = EXCLUDED.content_type, data = EXCLUDED.data, updated_at = now();

-- name: GetPersonAvatar :one
SELECT * FROM person_avatars WHERE person_id = @person_id;

-- name: DeletePersonAvatar :execrows
DELETE FROM person_avatars WHERE person_id = @person_id;
