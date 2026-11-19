-- +goose Up
CREATE TABLE teams (
 id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, lead_session TEXT NOT NULL REFERENCES sessions(id),
 status TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active','paused','stopped')),
 owner TEXT NOT NULL DEFAULT '', epoch INTEGER NOT NULL DEFAULT 0, lease_until INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE team_members (
 session_id TEXT PRIMARY KEY REFERENCES sessions(id), team_id TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
 name TEXT NOT NULL, model TEXT NOT NULL DEFAULT '', role TEXT NOT NULL CHECK(role IN ('lead','worker')), prompt TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL DEFAULT 'idle' CHECK(status IN ('idle','running','paused','stopped','failed')),
 turns INTEGER NOT NULL DEFAULT 0, max_turns INTEGER NOT NULL DEFAULT 30,
 mail_after INTEGER NOT NULL DEFAULT 0, UNIQUE(team_id,name)
);
CREATE TABLE team_tasks (
 id TEXT PRIMARY KEY, team_id TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
 title TEXT NOT NULL, prompt TEXT NOT NULL, assignee TEXT REFERENCES team_members(session_id),
 status TEXT NOT NULL CHECK(status IN ('pending','running','completed','failed','canceled','awaiting_approval')),
 result TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL
);
CREATE TABLE team_dependencies (
 task_id TEXT NOT NULL REFERENCES team_tasks(id) ON DELETE CASCADE,
 dependency_id TEXT NOT NULL REFERENCES team_tasks(id), PRIMARY KEY(task_id,dependency_id)
);
CREATE TABLE team_runs (
 id TEXT PRIMARY KEY, team_id TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
 session_id TEXT NOT NULL REFERENCES team_members(session_id), task_id TEXT REFERENCES team_tasks(id),
 mail_id TEXT UNIQUE REFERENCES session_mail(id) ON DELETE SET NULL,
 input TEXT NOT NULL, status TEXT NOT NULL CHECK(status IN ('running','completed','failed')),
 result TEXT NOT NULL DEFAULT '', epoch INTEGER NOT NULL, created_at INTEGER NOT NULL, finished_at INTEGER
);
CREATE INDEX team_tasks_ready ON team_tasks(team_id,status,created_at);
CREATE INDEX team_runs_member ON team_runs(session_id,created_at);
-- +goose Down
DROP TABLE team_runs;
DROP TABLE team_dependencies;
DROP TABLE team_tasks;
DROP TABLE team_members;
DROP TABLE teams;
