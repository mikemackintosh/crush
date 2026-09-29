-- +goose Up
-- +goose StatementBegin
-- A forked session remembers the session it was copied from, so the
-- sessions list can show forks under their source. Unlike
-- parent_session_id, which marks a sub-agent's task session and hides it
-- from the list, a fork is a top-level session in its own right.
ALTER TABLE sessions ADD COLUMN forked_from TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN forked_from;
-- +goose StatementEnd
