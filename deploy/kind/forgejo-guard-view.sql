-- Access of the Squared guard to the authentication sources of Forgejo.
--
-- Forgejo has no API for its authentication sources. A change in the
-- Forgejo web interface is only in the table login_source. The column cfg
-- of that table has the OAuth client secret, thus the guard does not get
-- the table. It gets this view, and the role squared_guard can read only
-- this view. The view has:
--
--   - for all sources: the name, the type, the active flag and the names
--     of the keys of cfg (never their values). The guard uses the key names
--     to find renamed or new fields after a Forgejo upgrade.
--   - for OAuth sources (type 6): the values of the fields that refer to
--     groups.
--
-- Run it as the owner of the Forgejo tables, after Forgejo made them. The
-- password of the role is set separately. If an upgrade of Forgejo must
-- change a column that the view uses, drop the view, upgrade, and run this
-- file again. The file works with PostgreSQL 13 and later.

DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'squared_guard') THEN
        CREATE ROLE squared_guard LOGIN;
    END IF;
END
$$;

-- A new column list needs a new view.
DROP VIEW IF EXISTS squared_guard_auth_sources;

CREATE VIEW squared_guard_auth_sources WITH (security_barrier) AS
WITH s AS (
    SELECT id, name, type, is_active,
           -- Only a JSON object has keys. Other values give NULL here. A
           -- broken JSON object stops the query, and the guard reports it.
           CASE WHEN cfg ~ '^\s*\{' THEN cfg::jsonb END AS c
    FROM login_source
)
SELECT id,
       name,
       type,
       is_active,
       CASE WHEN c IS NOT NULL THEN ARRAY(SELECT jsonb_object_keys(c) ORDER BY 1) END AS cfg_keys,
       CASE WHEN type = 6 THEN c ->> 'AdminGroup' END          AS admin_group,
       CASE WHEN type = 6 THEN c ->> 'RestrictedGroup' END     AS restricted_group,
       CASE WHEN type = 6 THEN c ->> 'GroupTeamMap' END        AS group_team_map,
       CASE WHEN type = 6 THEN c ->> 'GroupClaimName' END      AS group_claim_name,
       CASE WHEN type = 6 THEN c ->> 'RequiredClaimName' END   AS required_claim_name,
       CASE WHEN type = 6 THEN c ->> 'RequiredClaimValue' END  AS required_claim_value,
       CASE WHEN type = 6 THEN c ->> 'QuotaGroupClaimName' END AS quota_group_claim_name,
       CASE WHEN type = 6 THEN c ->> 'QuotaGroupMap' END       AS quota_group_map
FROM s;

REVOKE ALL ON squared_guard_auth_sources FROM PUBLIC;
GRANT SELECT ON squared_guard_auth_sources TO squared_guard;
