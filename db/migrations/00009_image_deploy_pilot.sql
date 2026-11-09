-- +goose Up
-- Keep approved-image deployment closed for every account until operators
-- explicitly admit that account to the pilot.
ALTER TABLE accounts
    ADD COLUMN image_deploy_enabled boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE accounts DROP COLUMN image_deploy_enabled;
