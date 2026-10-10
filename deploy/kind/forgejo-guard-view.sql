-- Access of the Squared guard to the authentication sources of Forgejo,
-- to the owners of personal access tokens and SSH keys, to the deploy keys
-- of the repositories, to the team and collaborator access, to the
-- protection of the default branches, and to the webhooks.
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
-- file again. The file works with PostgreSQL 13 and later (sha256 needs 11).

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

-- The users who have access to the code of the repositories of an
-- organization through a team or as collaborators. The guard checks that
-- only the users that Squared expects can push to the GitOps
-- repositories. Access mode: 1 read, 2 write, 3 admin, 4 owner. A team
-- with admin or owner access has it for all units; else the access to the
-- code is in team_unit (type 1). repos is "*" for a team with all
-- repositories of the organization. Forgejo site administrators are not in
-- the view.
DROP VIEW IF EXISTS squared_guard_repo_access;

CREATE VIEW squared_guard_repo_access WITH (security_barrier) AS
SELECT o.name                                   AS org_name,
       'team'                                   AS kind,
       t.name                                   AS via,
       u.name                                   AS user_name,
       CASE WHEN t.authorize >= 3 THEN t.authorize
            ELSE coalesce((SELECT max(tu.access_mode) FROM team_unit tu WHERE tu.team_id = t.id AND tu.type = 1), 0)
       END                                      AS mode,
       CASE WHEN t.includes_all_repositories THEN '*'
            ELSE coalesce((SELECT string_agg(r.name, ',' ORDER BY r.name)
                           FROM team_repo tr JOIN repository r ON r.id = tr.repo_id
                           WHERE tr.team_id = t.id), '')
       END                                      AS repos
FROM team t
JOIN "user" o ON o.id = t.org_id
JOIN team_user tm ON tm.team_id = t.id
JOIN "user" u ON u.id = tm.uid
UNION ALL
SELECT r.owner_name, 'collaborator', r.name, u.name, c.mode, r.name
FROM collaboration c
JOIN repository r ON r.id = c.repo_id
JOIN "user" u ON u.id = c.user_id;

REVOKE ALL ON squared_guard_repo_access FROM PUBLIC;
GRANT SELECT ON squared_guard_repo_access TO squared_guard;

-- The protection rule of the default branch of each repository: the rule
-- with the exact name of the branch, which Forgejo uses before a rule with
-- a pattern. A repository without such a rule is in the view too
-- (protected is false). The user lists have names, the team lists only
-- the number of teams. The guard checks that only Squared can push to the
-- GitOps repositories or merge into them. Forgejo refuses all pushes to an
-- archived repository.
DROP VIEW IF EXISTS squared_guard_branch_rules;

CREATE VIEW squared_guard_branch_rules WITH (security_barrier) AS
WITH p AS (
    SELECT repo_id, branch_name, can_push, enable_whitelist, whitelist_deploy_keys,
           enable_merge_whitelist, apply_to_admins,
           -- Forgejo writes the lists as JSON; an empty list can be NULL or "null".
           CASE WHEN whitelist_user_i_ds ~ '^\s*\[' THEN whitelist_user_i_ds::jsonb END             AS push_user_ids,
           CASE WHEN whitelist_team_i_ds ~ '^\s*\[' THEN whitelist_team_i_ds::jsonb END             AS push_team_ids,
           CASE WHEN merge_whitelist_user_i_ds ~ '^\s*\[' THEN merge_whitelist_user_i_ds::jsonb END AS merge_user_ids,
           CASE WHEN merge_whitelist_team_i_ds ~ '^\s*\[' THEN merge_whitelist_team_i_ds::jsonb END AS merge_team_ids
    FROM protected_branch
)
SELECT r.owner_name                                   AS owner_name,
       r.name                                         AS repo_name,
       r.default_branch                               AS branch,
       r.created_unix                                 AS repo_created_unix,
       r.is_archived                                  AS archived,
       p.repo_id IS NOT NULL                          AS protected,
       coalesce(p.can_push, false)                    AS can_push,
       coalesce(p.enable_whitelist, false)            AS enable_whitelist,
       ARRAY(SELECT u.name FROM "user" u
             WHERE u.id IN (SELECT jsonb_array_elements_text(p.push_user_ids)::bigint)
             ORDER BY u.name)                         AS push_users,
       coalesce(jsonb_array_length(p.push_team_ids), 0) AS push_teams,
       coalesce(p.whitelist_deploy_keys, false)       AS push_deploy_keys,
       coalesce(p.enable_merge_whitelist, false)      AS enable_merge_whitelist,
       ARRAY(SELECT u.name FROM "user" u
             WHERE u.id IN (SELECT jsonb_array_elements_text(p.merge_user_ids)::bigint)
             ORDER BY u.name)                         AS merge_users,
       coalesce(jsonb_array_length(p.merge_team_ids), 0) AS merge_teams,
       coalesce(p.apply_to_admins, false)             AS apply_to_admins
FROM repository r
LEFT JOIN p ON p.repo_id = r.id AND p.branch_name = r.default_branch;

REVOKE ALL ON squared_guard_branch_rules FROM PUBLIC;
GRANT SELECT ON squared_guard_branch_rules TO squared_guard;

-- The webhooks: of a repository, of an owner (organization or user), and
-- the system and default webhooks, which apply to all repositories. A
-- webhook sends the events of a repository (commit messages, file names,
-- authors) to its address, and Forgejo makes the request from inside the
-- cluster. The address can have a secret (a token in the path, or a user
-- and a password), thus the view has only its host and its SHA-256 hash,
-- never the address, the secret or the authorization header. The guard
-- compares the hash with the addresses that Squared expects.
DROP VIEW IF EXISTS squared_guard_webhooks;

CREATE VIEW squared_guard_webhooks WITH (security_barrier) AS
SELECT CASE WHEN w.repo_id > 0 THEN 'repository'
            WHEN w.owner_id > 0 THEN 'owner'
            WHEN w.is_system_webhook THEN 'system'
            ELSE 'default'
       END                                                         AS scope,
       coalesce(r.owner_name, o.name, '')                          AS owner_name,
       coalesce(r.name, '')                                        AS repo_name,
       w.type                                                      AS type,
       w.is_active                                                 AS active,
       encode(sha256(convert_to(w.url, 'UTF8')), 'hex')           AS url_sha256,
       lower(coalesce(substring(w.url from '^[A-Za-z][A-Za-z0-9+.-]*://(?:[^@/?#]*@)?([^/:?#]+)'), '')) AS url_host
FROM webhook w
LEFT JOIN repository r ON w.repo_id > 0 AND r.id = w.repo_id
LEFT JOIN "user" o ON w.repo_id = 0 AND w.owner_id > 0 AND o.id = w.owner_id;

REVOKE ALL ON squared_guard_webhooks FROM PUBLIC;
GRANT SELECT ON squared_guard_webhooks TO squared_guard;
