-- name: GetRoomById :one
SELECT * FROM rooms WHERE id = ?;

-- name: GetRoomByCode :one
SELECT * FROM rooms WHERE code = ?;

-- name: GetRoomMembers :many
SELECT *
FROM room_members rm
JOIN users ON users.id = rm.user_id
WHERE rm.room_id = ?;

---------------------------
-- CREATE, UPDATE, DELETE
---------------------------
-- name: CreateRoom :one
INSERT INTO rooms (name, host_id, max_players, max_locations, code, timer_duration)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: UpdateRoomHost :exec
UPDATE rooms SET host_id = ? WHERE id = ?;

-- name: UpdateRoomState :exec
UPDATE rooms SET state = ? WHERE id = ?;

-- name: UpdateRoomStateIf :execresult
UPDATE rooms SET state = ? WHERE id = ? AND state = ?;

-- name: JoinRoom :exec
INSERT OR IGNORE INTO room_members (room_id, user_id) VALUES (?, ?);

-- JoinRoomIfNotFull atomically inserts the member only if the room is below its
-- max_players. Returns the number of rows inserted (0 = room was full). The
-- count check and insert are one statement, so concurrent joins can't exceed
-- the cap. Callers should only use this for users not already in the room
-- (an existing member also yields 0 rows via INSERT OR IGNORE).
-- name: JoinRoomIfNotFull :execrows
INSERT OR IGNORE INTO room_members (room_id, user_id)
SELECT sqlc.arg(room_id), sqlc.arg(user_id)
WHERE (
    SELECT COUNT(*) FROM room_members WHERE room_id = sqlc.arg(room_id)
) < (
    SELECT max_players FROM rooms WHERE id = sqlc.arg(room_id)
);

-- name: LeaveRoom :exec
DELETE FROM room_members WHERE room_id = ? AND user_id = ?;

-- name: AddPointsToMember :exec
UPDATE room_members SET points = points + ? WHERE room_id = ? AND user_id = ?;

-- name: AddPointsToAllExcept :exec
UPDATE room_members SET points = points + ? WHERE room_id = ? AND user_id != ?;

-- name: DeleteOldRooms :exec
DELETE FROM rooms WHERE created_at < unixepoch() - (12 * 3600);

