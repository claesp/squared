-- Access of the Squared guard to the authentication sources of Forgejo,
-- to the owners of personal access tokens and SSH keys, and to the deploy
-- keys of the repositories.
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
-- change a column that a view uses, drop the views, upgrade, and run this
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

-- The owners of credentials that work without the identity provider,
-- among the users of OAuth sources: personal access tokens (API and Git)
-- and SSH keys of the user (type 1) or principals (type 3), which Git over
-- SSH accepts. Deploy keys (type 2) belong to repositories. The guard
-- compares each owner with the user in Keycloak. The view has the number
-- of the credentials and the time of the last use, never the tokens (or
-- their hashes, salts or last characters) or the keys. Local accounts are
-- not in the view.
DROP VIEW IF EXISTS squared_guard_token_owners;
DROP VIEW IF EXISTS squared_guard_credential_owners;

CREATE VIEW squared_guard_credential_owners WITH (security_barrier) AS
WITH tokens AS (
    SELECT uid, count(*) AS n, max(updated_unix) AS last_used
    FROM access_token
    GROUP BY uid
), ssh_keys AS (
    SELECT owner_id AS uid, count(*) AS n, max(updated_unix) AS last_used
    FROM public_key
    WHERE type IN (1, 3)
    GROUP BY owner_id
)
SELECT u.name                          AS user_name,
       u.login_source                  AS login_source,
       u.login_name                    AS login_name,
       u.is_admin                      AS is_admin,
       u.is_active                     AS is_active,
       u.prohibit_login                AS prohibit_login,
       coalesce(t.n, 0)                AS tokens,
       coalesce(t.last_used, 0)        AS tokens_last_used_unix,
       coalesce(k.n, 0)                AS ssh_keys,
       coalesce(k.last_used, 0)        AS ssh_keys_last_used_unix
FROM "user" u
JOIN login_source s ON s.id = u.login_source AND s.type = 6
LEFT JOIN tokens t ON t.uid = u.id
LEFT JOIN ssh_keys k ON k.uid = u.id
WHERE u.type = 0 -- Individual users, not organizations.
  AND (t.n IS NOT NULL OR k.n IS NOT NULL);

REVOKE ALL ON squared_guard_credential_owners FROM PUBLIC;
GRANT SELECT ON squared_guard_credential_owners TO squared_guard;

-- The deploy keys of all repositories. A deploy key gives Git over SSH
-- access to one repository, without a user and without the identity
-- provider; a key with write access (mode 2) can push. The guard checks
-- that nothing but Squared can write to the GitOps repositories. The view
-- has the repository, the name, the access and the last use of each key,
-- never the key or its fingerprint.
DROP VIEW IF EXISTS squared_guard_deploy_keys;

CREATE VIEW squared_guard_deploy_keys WITH (security_barrier) AS
SELECT r.owner_name                                            AS owner_name,
       r.name                                                  AS repo_name,
       d.name                                                  AS key_name,
       d.mode                                                  AS mode,
       d.created_unix                                          AS created_unix,
       greatest(d.updated_unix, coalesce(k.updated_unix, 0))   AS last_used_unix
FROM deploy_key d
JOIN repository r ON r.id = d.repo_id
LEFT JOIN public_key k ON k.id = d.key_id;

REVOKE ALL ON squared_guard_deploy_keys FROM PUBLIC;
GRANT SELECT ON squared_guard_deploy_keys TO squared_guard;
