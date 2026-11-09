-- +goose Up
ALTER TABLE agent_commands ADD COLUMN engine text;
ALTER TABLE agent_commands DROP CONSTRAINT agent_commands_type_check;
ALTER TABLE agent_commands ADD CONSTRAINT agent_commands_type_check
    CHECK (type IN ('run_now','pause','resume','update_config','update_agent','list_servers'));
ALTER TABLE agent_commands ADD CONSTRAINT agent_commands_server_engine_check CHECK (
    (type = 'list_servers' AND engine IS NOT NULL AND engine IN ('ookla','librespeed')) OR
    (type <> 'list_servers' AND engine IS NULL)
);

CREATE TABLE node_engine_servers (
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    engine text NOT NULL CHECK (engine IN ('ookla','librespeed')),
    servers jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(servers) = 'array'),
    pinned_server_id bigint CHECK (pinned_server_id > 0),
    refreshed_at timestamptz,
    PRIMARY KEY (node_id, engine)
);

-- Preserve the legacy primary-engine selection while the catalog is refreshed.
INSERT INTO node_engine_servers (node_id, engine, pinned_server_id)
SELECT id, primary_engine, server_id FROM nodes
WHERE server_id IS NOT NULL
ON CONFLICT (node_id, engine) DO NOTHING;

-- +goose Down
DELETE FROM agent_commands WHERE type = 'list_servers';
DROP TABLE node_engine_servers;
ALTER TABLE agent_commands DROP CONSTRAINT agent_commands_server_engine_check;
ALTER TABLE agent_commands DROP CONSTRAINT agent_commands_type_check;
ALTER TABLE agent_commands ADD CONSTRAINT agent_commands_type_check
    CHECK (type IN ('run_now','pause','resume','update_config','update_agent'));
ALTER TABLE agent_commands DROP COLUMN engine;
