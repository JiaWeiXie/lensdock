package feedback

import "github.com/JiaWeiXie/lensdock/internal/storage"

// migrations are the forward-only schema steps of the feedback component.
// Published revisions and their artifacts are protected by triggers: the
// database itself refuses to rewrite or delete them.
var migrations = []storage.Migration{{
	Version: 1,
	Name:    "captures, feedback revisions, artifacts and deliveries",
	Statements: []string{
		`CREATE TABLE captures (
			id TEXT PRIMARY KEY,
			workspace_id TEXT NOT NULL,
			owner_id TEXT NOT NULL,
			source TEXT NOT NULL CHECK (source IN ('desktop','page-viewport','page-full')),
			provenance TEXT NOT NULL CHECK (provenance IN ('client-framebuffer','runtime-cdp')),
			status TEXT NOT NULL CHECK (status IN ('awaiting-upload','capturing','ready','failed')),
			consistency TEXT NOT NULL CHECK (consistency IN ('stable-navigation','best-effort','unknown')),
			runtime_generation TEXT NOT NULL,
			target_id TEXT,
			navigation_id TEXT,
			lease_id TEXT,
			prior_lease_id TEXT,
			captured_at INTEGER,
			server_received_at INTEGER,
			width INTEGER NOT NULL DEFAULT 0,
			height INTEGER NOT NULL DEFAULT 0,
			page_json TEXT,
			parent_json TEXT,
			object_key TEXT,
			sha256 TEXT,
			size_bytes INTEGER NOT NULL DEFAULT 0,
			feedback_id TEXT,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL
		)`,
		`CREATE INDEX captures_owner ON captures (workspace_id, owner_id)`,
		`CREATE TABLE feedback (
			id TEXT PRIMARY KEY,
			workspace_id TEXT NOT NULL,
			created_by TEXT NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE revisions (
			feedback_id TEXT NOT NULL REFERENCES feedback (id),
			revision INTEGER NOT NULL CHECK (revision >= 1),
			owner_id TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('DRAFT','PUBLISHED')),
			capture_id TEXT NOT NULL,
			task_json TEXT NOT NULL,
			annotations_json TEXT NOT NULL,
			version INTEGER NOT NULL,
			preview_key TEXT,
			preview_size INTEGER NOT NULL DEFAULT 0,
			preview_version INTEGER NOT NULL DEFAULT 0,
			title TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			warned INTEGER NOT NULL DEFAULT 0,
			published_at INTEGER,
			publisher_id TEXT,
			manifest_sha256 TEXT,
			redacted_sha256 TEXT,
			PRIMARY KEY (feedback_id, revision)
		)`,
		`CREATE INDEX revisions_status ON revisions (status, expires_at)`,
		`CREATE TABLE artifacts (
			feedback_id TEXT NOT NULL,
			revision INTEGER NOT NULL,
			name TEXT NOT NULL,
			mime_type TEXT NOT NULL,
			sha256 TEXT NOT NULL,
			size_bytes INTEGER NOT NULL,
			PRIMARY KEY (feedback_id, revision, name),
			FOREIGN KEY (feedback_id, revision) REFERENCES revisions (feedback_id, revision)
		)`,
		`CREATE TABLE deliveries (
			seq INTEGER PRIMARY KEY AUTOINCREMENT,
			feedback_id TEXT NOT NULL,
			revision INTEGER NOT NULL,
			workspace_id TEXT NOT NULL,
			state TEXT NOT NULL CHECK (state IN ('READY','QUEUED','ACKNOWLEDGED','IN_PROGRESS','RESOLVED','VERIFIED','CANCELLED')),
			agent_id TEXT NOT NULL DEFAULT '',
			queued_at INTEGER,
			acknowledged_at INTEGER,
			in_progress_at INTEGER,
			resolved_at INTEGER,
			verified_at INTEGER,
			cancelled_at INTEGER,
			lease_id TEXT NOT NULL DEFAULT '',
			summary TEXT NOT NULL DEFAULT '',
			verification TEXT NOT NULL DEFAULT '',
			result_ids_json TEXT NOT NULL DEFAULT '[]',
			last_reason TEXT NOT NULL DEFAULT '',
			rejection_count INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			UNIQUE (feedback_id, revision),
			FOREIGN KEY (feedback_id, revision) REFERENCES revisions (feedback_id, revision)
		)`,
		`CREATE INDEX deliveries_workspace ON deliveries (workspace_id, seq)`,
		`CREATE INDEX deliveries_agent ON deliveries (agent_id, state)`,
		`CREATE TABLE delivery_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			feedback_id TEXT NOT NULL,
			revision INTEGER NOT NULL,
			from_state TEXT NOT NULL,
			to_state TEXT NOT NULL,
			actor_id TEXT NOT NULL,
			actor_role TEXT NOT NULL,
			at INTEGER NOT NULL,
			reason TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX delivery_history_key ON delivery_history (feedback_id, revision, id)`,
		`CREATE TRIGGER delivery_history_no_update BEFORE UPDATE ON delivery_history BEGIN SELECT RAISE(ABORT, 'delivery history is append-only'); END`,
		`CREATE TRIGGER delivery_history_no_delete BEFORE DELETE ON delivery_history BEGIN SELECT RAISE(ABORT, 'delivery history is append-only'); END`,
		`CREATE TRIGGER revisions_published_immutable
			BEFORE UPDATE OF feedback_id, revision, owner_id, status, capture_id, task_json, annotations_json, title, published_at, publisher_id, manifest_sha256, redacted_sha256
			ON revisions WHEN OLD.status = 'PUBLISHED'
			BEGIN SELECT RAISE(ABORT, 'published revisions are immutable'); END`,
		`CREATE TRIGGER revisions_published_no_delete
			BEFORE DELETE ON revisions WHEN OLD.status = 'PUBLISHED'
			BEGIN SELECT RAISE(ABORT, 'published revisions cannot be deleted'); END`,
		`CREATE TRIGGER artifacts_no_update BEFORE UPDATE ON artifacts BEGIN SELECT RAISE(ABORT, 'artifacts are immutable'); END`,
		`CREATE TRIGGER artifacts_no_delete BEFORE DELETE ON artifacts BEGIN SELECT RAISE(ABORT, 'artifacts are immutable'); END`,
	},
}, {
	// Standalone drawings: widen the captures CHECK constraints. SQLite cannot
	// alter a CHECK, so the table is rebuilt from migration 1's definition with
	// every column, row and the index preserved. No other table references
	// captures (revisions.capture_id is a plain column), so the published
	// revisions, artifacts, deliveries and their immutability triggers are not
	// touched. The two table CHECKs at the end tie a drawing to its provenance
	// and keep it free of any runtime, lease, target or page context.
	Version: 2,
	Name:    "standalone drawing captures",
	Statements: []string{
		`CREATE TABLE captures_v2 (
			id TEXT PRIMARY KEY,
			workspace_id TEXT NOT NULL,
			owner_id TEXT NOT NULL,
			source TEXT NOT NULL CHECK (source IN ('desktop','page-viewport','page-full','drawing')),
			provenance TEXT NOT NULL CHECK (provenance IN ('client-framebuffer','runtime-cdp','generated-canvas')),
			status TEXT NOT NULL CHECK (status IN ('awaiting-upload','capturing','ready','failed')),
			consistency TEXT NOT NULL CHECK (consistency IN ('stable-navigation','best-effort','unknown')),
			runtime_generation TEXT NOT NULL,
			target_id TEXT,
			navigation_id TEXT,
			lease_id TEXT,
			prior_lease_id TEXT,
			captured_at INTEGER,
			server_received_at INTEGER,
			width INTEGER NOT NULL DEFAULT 0,
			height INTEGER NOT NULL DEFAULT 0,
			page_json TEXT,
			parent_json TEXT,
			object_key TEXT,
			sha256 TEXT,
			size_bytes INTEGER NOT NULL DEFAULT 0,
			feedback_id TEXT,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			CHECK ((source = 'drawing') = (provenance = 'generated-canvas')),
			CHECK (source <> 'drawing' OR (runtime_generation = '' AND consistency = 'unknown'
				AND target_id IS NULL AND navigation_id IS NULL AND lease_id IS NULL AND prior_lease_id IS NULL AND page_json IS NULL))
		)`,
		`INSERT INTO captures_v2 (id, workspace_id, owner_id, source, provenance, status, consistency, runtime_generation,
			target_id, navigation_id, lease_id, prior_lease_id, captured_at, server_received_at, width, height,
			page_json, parent_json, object_key, sha256, size_bytes, feedback_id, created_at, updated_at, expires_at)
			SELECT id, workspace_id, owner_id, source, provenance, status, consistency, runtime_generation,
			target_id, navigation_id, lease_id, prior_lease_id, captured_at, server_received_at, width, height,
			page_json, parent_json, object_key, sha256, size_bytes, feedback_id, created_at, updated_at, expires_at
			FROM captures`,
		`DROP TABLE captures`,
		`ALTER TABLE captures_v2 RENAME TO captures`,
		`CREATE INDEX captures_owner ON captures (workspace_id, owner_id)`,
	},
}, {
	// Published-history clearing: a revision listed here is hidden from the
	// history listing (GET /feedback?history=visible) only. The published
	// revision, its artifacts and its delivery are not touched, so the
	// immutability triggers of migration 1 stay untouched; rows are only ever
	// inserted. A v2 Gateway refuses this schema (rollback needs the snapshot).
	Version: 3,
	Name:    "hidden published history",
	Statements: []string{
		`CREATE TABLE history_hidden (
			feedback_id TEXT NOT NULL,
			revision INTEGER NOT NULL,
			workspace_id TEXT NOT NULL,
			hidden_by TEXT NOT NULL,
			hidden_at INTEGER NOT NULL,
			PRIMARY KEY (feedback_id, revision),
			FOREIGN KEY (feedback_id, revision) REFERENCES revisions (feedback_id, revision)
		)`,
		`CREATE INDEX history_hidden_workspace ON history_hidden (workspace_id)`,
	},
}}
