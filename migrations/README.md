# Migrations

Database migrations for the `speedtest` schema live here. Migrations are plain
SQL files applied in lexical order by filename; a later ticket adds the tool
that runs them.

- Numbered with a zero-padded prefix and a short slug, e.g. `0001_create_nodes.sql`.
- Every migration must have a matching rollback; write it down next to the
  forward migration.
- Migrations are applied to `speedtest_test` or the worktree database during
  development, never to the live `speedtest` database directly.
