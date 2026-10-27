CREATE TABLE tenant_usage (
    tenant_id text PRIMARY KEY REFERENCES tenants(id),
    automations bigint NOT NULL DEFAULT 0 CHECK (automations >= 0),
    events bigint NOT NULL DEFAULT 0 CHECK (events >= 0),
    receipts bigint NOT NULL DEFAULT 0 CHECK (receipts >= 0)
);
INSERT INTO tenant_usage(tenant_id,automations,events,receipts)
SELECT t.id,
    (SELECT count(*) FROM automations a WHERE a.tenant_id=t.id),
    (SELECT count(*) FROM inbound_events e WHERE e.tenant_id=t.id),
    (SELECT count(*) FROM event_receipts r WHERE r.tenant_id=t.id)
FROM tenants t;
CREATE INDEX automations_page ON automations(tenant_id,created_at,id);
