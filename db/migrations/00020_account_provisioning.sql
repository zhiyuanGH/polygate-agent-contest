-- +goose Up
ALTER TABLE accounts ADD COLUMN provisioning_state text NOT NULL DEFAULT 'Active'
    CHECK (provisioning_state IN ('Available', 'Creating', 'Active'));
ALTER TABLE accounts ADD COLUMN provision_claim_token uuid;
ALTER TABLE accounts ADD COLUMN provision_claimed_at timestamptz;
ALTER TABLE accounts ADD CONSTRAINT account_provision_state_consistent CHECK (
    provisioning_state = 'Active' OR NOT enabled);
CREATE INDEX account_available_slots_idx ON accounts (id) WHERE provisioning_state = 'Available';
ALTER TABLE admin_account_audit DROP CONSTRAINT admin_account_audit_action_check;
ALTER TABLE admin_account_audit ADD CONSTRAINT admin_account_audit_action_check
    CHECK (action IN ('ENABLE_ACCOUNT', 'DISABLE_ACCOUNT', 'CREATE_ACCOUNT', 'RESET_PASSWORD'));

-- +goose Down
ALTER TABLE admin_account_audit DROP CONSTRAINT admin_account_audit_action_check;
ALTER TABLE admin_account_audit ADD CONSTRAINT admin_account_audit_action_check
    CHECK (action IN ('ENABLE_ACCOUNT', 'DISABLE_ACCOUNT'));
DROP INDEX account_available_slots_idx;
ALTER TABLE accounts DROP CONSTRAINT account_provision_state_consistent;
ALTER TABLE accounts DROP COLUMN provision_claimed_at;
ALTER TABLE accounts DROP COLUMN provision_claim_token;
ALTER TABLE accounts DROP COLUMN provisioning_state;
