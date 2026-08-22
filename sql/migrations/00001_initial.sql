-- +goose Up
CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    username TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    tripcode TEXT NOT NULL UNIQUE,
    created_at INTEGER NOT NULL DEFAULT (unixepoch()),
    crab_avatar TEXT NOT NULL DEFAULT 'tourist-crab.png'
);

CREATE TABLE rooms (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    code TEXT NOT NULL UNIQUE,
    host_id INTEGER NOT NULL REFERENCES users(id),
    max_locations INTEGER NOT NULL DEFAULT 30,
    max_players INTEGER NOT NULL DEFAULT 8,
    created_at INTEGER NOT NULL DEFAULT (unixepoch()),
    state TEXT NOT NULL DEFAULT 'lobby' CHECK (state IN (
        'lobby',
        'game',
        'reveal',
        'timeup',
        'finish',
        'closed'
    )),
    timer_duration INTEGER NOT NULL DEFAULT 480
);

CREATE TABLE room_members (
    room_id INTEGER NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    joined_at INTEGER NOT NULL DEFAULT (unixepoch()),
    is_ready INTEGER NOT NULL DEFAULT 0,
    points INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (room_id, user_id)
);

CREATE TABLE games (
    id INTEGER PRIMARY KEY,
    room_id INTEGER NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    spy_id INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    location TEXT NOT NULL,
    location_pool TEXT NOT NULL DEFAULT '[]',
    role_assignments TEXT NOT NULL DEFAULT '{}',
    started_at INTEGER NOT NULL DEFAULT (unixepoch()),
    timer_duration INTEGER NOT NULL DEFAULT 480
);

CREATE TABLE game_events (
    id INTEGER PRIMARY KEY,
    game_id INTEGER NOT NULL REFERENCES games(id) ON DELETE CASCADE,
    user_id INTEGER NOT NULL REFERENCES users(id),
    event_type TEXT NOT NULL CHECK(event_type IN (
        'paused',
        'unpaused',
        'accused',
        'voted',
        'game_started',
        'game_finished',
        'location_revealed',
        'location_guessed'
    )),
    target_id INTEGER REFERENCES users(id),
    metadata TEXT,
    created_at INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE INDEX idx_room_members_user ON room_members(user_id);
CREATE INDEX idx_games_room ON games(room_id);
CREATE INDEX idx_game_events_game_id ON game_events(game_id);
