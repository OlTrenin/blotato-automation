CREATE TABLE tenants (
    id text PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 200)
);

CREATE TABLE connected_accounts (
    tenant_id text NOT NULL REFERENCES tenants(id),
    id text NOT NULL,
    platform text NOT NULL CHECK (platform = 'instagram'),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, id, platform)
);

CREATE TABLE automations (
    id text PRIMARY KEY,
    tenant_id text NOT NULL,
    account_id text NOT NULL,
    platform text NOT NULL CHECK (platform = 'instagram'),
    keyword text NOT NULL CHECK (length(keyword) > 0),
    comment_reply text NOT NULL,
    email_prompt text NOT NULL,
    link text NOT NULL,
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, account_id, platform) REFERENCES connected_accounts(tenant_id, id, platform)
);
CREATE INDEX automations_matching ON automations(tenant_id, account_id, platform, created_at, id) WHERE enabled;

CREATE TABLE executions (
    id text PRIMARY KEY,
    tenant_id text NOT NULL,
    automation_id text NOT NULL,
    account_id text NOT NULL,
    platform text NOT NULL CHECK (platform = 'instagram'),
    user_id text NOT NULL,
    comment_id text NOT NULL,
    state text NOT NULL CHECK (state IN ('INITIAL_ACTIONS_PENDING', 'WAITING_FOR_EMAIL', 'FINAL_MESSAGE_PENDING', 'COMPLETED', 'FAILED', 'EXPIRED')),
    snapshot jsonb NOT NULL,
    email text,
    failure text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz,
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, automation_id) REFERENCES automations(tenant_id, id),
    FOREIGN KEY (tenant_id, account_id, platform) REFERENCES connected_accounts(tenant_id, id, platform),
    CHECK (state <> 'WAITING_FOR_EMAIL' OR expires_at IS NOT NULL),
    CHECK (state NOT IN ('FINAL_MESSAGE_PENDING', 'COMPLETED') OR email IS NOT NULL)
);
CREATE UNIQUE INDEX executions_one_active ON executions(tenant_id, account_id, platform, user_id)
    WHERE state IN ('INITIAL_ACTIONS_PENDING', 'WAITING_FOR_EMAIL', 'FINAL_MESSAGE_PENDING');
CREATE INDEX executions_tenant_created ON executions(tenant_id, created_at, id);
CREATE INDEX executions_expiry ON executions(expires_at) WHERE state = 'WAITING_FOR_EMAIL';

-- Canonical logical object; content hashes avoid retaining raw inbound DM text.
CREATE TABLE inbound_events (
    id text PRIMARY KEY,
    tenant_id text NOT NULL,
    account_id text NOT NULL,
    platform text NOT NULL CHECK (platform = 'instagram'),
    kind text NOT NULL CHECK (kind IN ('comment', 'message')),
    object_id text NOT NULL,
    user_id text NOT NULL,
    content_hash text NOT NULL,
    outcome text NOT NULL,
    execution_id text,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (tenant_id, account_id, kind, object_id),
    UNIQUE (tenant_id, account_id, kind, id),
    FOREIGN KEY (tenant_id, account_id, platform) REFERENCES connected_accounts(tenant_id, id, platform),
    FOREIGN KEY (tenant_id, execution_id) REFERENCES executions(tenant_id, id)
);
CREATE TABLE event_receipts (
    tenant_id text NOT NULL,
    account_id text NOT NULL,
    kind text NOT NULL,
    event_id text NOT NULL,
    inbound_id text NOT NULL,
    content_hash text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_id, account_id, kind, event_id),
    FOREIGN KEY (tenant_id, account_id, kind, inbound_id) REFERENCES inbound_events(tenant_id, account_id, kind, id)
);

CREATE TABLE actions (
    id text PRIMARY KEY,
    execution_id text NOT NULL,
    tenant_id text NOT NULL,
    account_id text NOT NULL,
    platform text NOT NULL CHECK (platform = 'instagram'),
    user_id text NOT NULL,
    comment_id text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('reply_comment', 'request_email', 'send_link')),
    body text NOT NULL,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'delivered', 'failed')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    available_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    lease_token text,
    lease_until timestamptz,
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    delivered_at timestamptz,
    UNIQUE (execution_id, kind),
    FOREIGN KEY (tenant_id, execution_id) REFERENCES executions(tenant_id, id),
    FOREIGN KEY (tenant_id, account_id, platform) REFERENCES connected_accounts(tenant_id, id, platform),
    CHECK ((status = 'processing') = (lease_token IS NOT NULL AND lease_until IS NOT NULL))
);
CREATE INDEX actions_claim ON actions(available_at, id) WHERE status IN ('pending', 'processing');

-- Simulated remote provider storage: deliberately no foreign key to the outbox.
CREATE TABLE simulator_effects (
    action_id text PRIMARY KEY,
    execution_id text NOT NULL,
    tenant_id text NOT NULL,
    account_id text NOT NULL,
    user_id text NOT NULL,
    comment_id text NOT NULL,
    kind text NOT NULL,
    body text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX simulator_effects_tenant_created ON simulator_effects(tenant_id, created_at, action_id);
CREATE TABLE simulator_failures (
    tenant_id text NOT NULL REFERENCES tenants(id),
    kind text NOT NULL CHECK (kind IN ('reply_comment', 'request_email', 'send_link')),
    mode text NOT NULL CHECK (mode IN ('transient', 'permanent', 'after_success')),
    remaining integer NOT NULL CHECK (remaining BETWEEN 0 AND 20),
    PRIMARY KEY (tenant_id, kind)
);
