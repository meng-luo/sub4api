ALTER TABLE codex_ticket_attempts
    ADD COLUMN IF NOT EXISTS upstream_error_code VARCHAR(128),
    ADD COLUMN IF NOT EXISTS upstream_error_message VARCHAR(1024);
