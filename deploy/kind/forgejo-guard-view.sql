-- Access of the Squared guard to the authentication sources of Forgejo.
--
-- Forgejo has no API for its authentication sources. A change in the
-- Forgejo web interface is only in the table login_source. The column cfg
-- of that table has the OAuth client secret, thus the guard does not get
-- the table. It gets this view, with only the group settings of the OAuth
-- sources (type 6), and the role squared_guard can read only this view.
--
-- Run it as the owner of the Forgejo tables, after Forgejo made them. The
-- password of the role is set separately. If an upgrade of Forgejo must
-- change a column that the view uses, drop the view, upgrade, and run this
-- file again.

DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'squared_guard') THEN
        CREATE ROLE squared_guard LOGIN;
    END IF;
END
$$;

CREATE OR REPLACE VIEW squared_guard_auth_sources WITH (security_barrier) AS
SELECT id,
       name,
       is_active,
       coalesce(cfg::jsonb ->> 'AdminGroup', '')      AS admin_group,
       coalesce(cfg::jsonb ->> 'RestrictedGroup', '') AS restricted_group,
       coalesce(cfg::jsonb ->> 'GroupTeamMap', '')    AS group_team_map
FROM login_source
WHERE type = 6;

REVOKE ALL ON squared_guard_auth_sources FROM PUBLIC;
GRANT SELECT ON squared_guard_auth_sources TO squared_guard;
