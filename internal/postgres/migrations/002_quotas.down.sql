DROP INDEX automations_page;
DROP TABLE tenant_usage;
DELETE FROM schema_migrations WHERE version=2;
