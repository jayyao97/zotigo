-- add column "position" to table: "projects"
ALTER TABLE `projects` ADD COLUMN `position` integer NULL;
-- add column "pinned_at" to table: "projects"
ALTER TABLE `projects` ADD COLUMN `pinned_at` integer NULL;
-- add column "pinned_position" to table: "projects"
ALTER TABLE `projects` ADD COLUMN `pinned_position` integer NULL;
-- add column "position" to table: "workspaces"
ALTER TABLE `workspaces` ADD COLUMN `position` integer NULL;
-- add column "pinned_at" to table: "workspaces"
ALTER TABLE `workspaces` ADD COLUMN `pinned_at` integer NULL;
-- add column "pinned_position" to table: "workspaces"
ALTER TABLE `workspaces` ADD COLUMN `pinned_position` integer NULL;
-- create "navigation_migration" table
CREATE TABLE `navigation_migration` (`singleton` integer NULL, PRIMARY KEY (`singleton`), CHECK (singleton = 1));

-- Keep old binaries and read-only clients aware of the schema version.
UPDATE schema_meta SET version = 8 WHERE singleton = 1;
