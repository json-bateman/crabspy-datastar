-- name: GetUserById :one
SELECT * FROM users
WHERE id = ?
LIMIT 1;

-- name: GetUserByTripcode :one
SELECT * FROM users
WHERE tripcode = ?
LIMIT 1;

---------------------------
-- CREATE, UPDATE, DELETE
---------------------------
-- name: CreateUser :one
INSERT INTO users (username, display_name, tripcode)
VALUES (?, ?, ?)
RETURNING *;

-- name: UpdateUserName :one
UPDATE users SET username = ?, display_name = ? WHERE id = ? RETURNING *;

-- name: UpdateUserAvatar :one
UPDATE users SET crab_avatar = ? WHERE id = ? RETURNING *;
