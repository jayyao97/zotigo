-- reverse: create "navigation_migration" table
DROP TABLE `navigation_migration`;
-- reverse: add column "pinned_position" to table: "workspaces"
ALTER TABLE `workspaces` DROP COLUMN `pinned_position`;
-- reverse: add column "pinned_at" to table: "workspaces"
ALTER TABLE `workspaces` DROP COLUMN `pinned_at`;
-- reverse: add column "position" to table: "workspaces"
ALTER TABLE `workspaces` DROP COLUMN `position`;
-- reverse: add column "pinned_position" to table: "projects"
ALTER TABLE `projects` DROP COLUMN `pinned_position`;
-- reverse: add column "pinned_at" to table: "projects"
ALTER TABLE `projects` DROP COLUMN `pinned_at`;
-- reverse: add column "position" to table: "projects"
ALTER TABLE `projects` DROP COLUMN `position`;

-- Keep old binaries and read-only clients aware of the schema version.
UPDATE schema_meta SET version = 7 WHERE singleton = 1;
