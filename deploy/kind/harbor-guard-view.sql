-- Access of the Squared guard to the configuration of Harbor.
--
-- Harbor keeps the settings of the web interface and of the API in the
-- table properties (one row for each key). Only the system administrator
-- can read them through the API, and the table has secrets, for example
-- oidc_client_secret. Thus the guard does not get the table. It gets this
-- view, and the role squared_guard can read only this view. The view has:
--
--   - all keys (never their values). The guard uses the keys to find new
--     or renamed settings after a Harbor upgrade.
--   - the values of the settings that refer to groups. All other values
--     are NULL.
--
-- Run it as the owner of the Harbor tables (postgres), in the database
-- registry, after Harbor made its tables. The password of the role is set
-- separately. If an upgrade of Harbor must change the table properties,
-- drop the view, upgrade, and run this file again. The file works with
-- PostgreSQL 13 and later.

DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'squared_guard') THEN
        CREATE ROLE squared_guard LOGIN;
    END IF;
END
$$;

-- A new column list needs a new view.
DROP VIEW IF EXISTS squared_guard_harbor_config;

CREATE VIEW squared_guard_harbor_config WITH (security_barrier) AS
SELECT k AS key,
       CASE WHEN k IN ('auth_mode', 'primary_auth_mode', 'oidc_admin_group', 'oidc_groups_claim',
                       'oidc_group_filter', 'http_authproxy_admin_groups')
            THEN v END AS value
FROM properties;

REVOKE ALL ON squared_guard_harbor_config FROM PUBLIC;
GRANT SELECT ON squared_guard_harbor_config TO squared_guard;
