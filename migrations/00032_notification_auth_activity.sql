-- +goose Up
CREATE TABLE notification_login_failures (
    id bigserial PRIMARY KEY,
    ip inet NOT NULL,
    attempted_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notification_login_failures_ip_time_idx ON notification_login_failures(ip, attempted_at DESC);

CREATE TABLE notification_signin_locations (
    user_key text NOT NULL,
    ip inet NOT NULL,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(user_key, ip)
);

-- +goose Down
DROP TABLE notification_signin_locations;
DROP TABLE notification_login_failures;
