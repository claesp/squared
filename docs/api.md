# API

Squared has a JSON API at `/api/v1`. The API has the same operations and
the same permission checks as the web interface.

## Authentication

The API accepts two kinds of bearer tokens. Send the token in each
request:

```
Authorization: Bearer <token>
```

| Token | How to get it | Lifetime |
|---|---|---|
| Personal access token (`sqp_...`) | **Account** page of the web interface. | Until it expires or you remove it. |
| Keycloak access token | Any OAuth flow of Keycloak for a client that adds the audience `squared-api` to its access tokens. | Short, for example five minutes. |

The API does not accept session cookies. A token has the permissions of
its user. When an administrator disables the user, the token stops
working. A Keycloak token of a person who did not sign in before makes the
Squared account, as the first web sign-in does.

Example with the development environment (the password grant of the
`squared-cli` client exists only there):

```bash
TOKEN=$(curl -s http://localhost:30180/realms/squared/protocol/openid-connect/token \
  -d grant_type=password -d client_id=squared-cli -d scope=openid \
  -d username=dev --data-urlencode "password=$(cat .dev/keycloak-dev-user-password)" | jq -r .access_token)
curl -H "Authorization: Bearer $TOKEN" http://localhost:30080/api/v1/me
```

## Conventions

- Request and response bodies are JSON (`Content-Type: application/json`).
- The server rejects unknown fields in request bodies.
- Times use RFC 3339 in UTC.
- A list response has the form `{"items": [...]}`.
- Changes that the worker completes later return `202 Accepted`, or the
  changed object. Read the object again to see the `status`.

## Errors

All errors have this form:

```json
{
  "error": {
    "code": "invalid",
    "message": "the input is not valid",
    "fields": [{"field": "image", "message": "is required"}]
  }
}
```

| Status | `code` | Meaning |
|---|---|---|
| 400 | `bad_request` | The body is not valid JSON, or it has unknown fields. |
| 401 | `unauthenticated` | The token is missing or not valid. |
| 403 | `forbidden` | Your role does not permit the operation. |
| 404 | `not_found` | The object does not exist, or you are not a member of its tenant. |
| 409 | `conflict` | The operation is not possible now, for example a change to a deployment that is being removed. |
| 422 | `invalid` | One or more fields are not valid. `fields` tells which. |
| 500 | `internal` | An internal error. The server log has the details. |

## Endpoints

| Method and path | Role | Description |
|---|---|---|
| `GET /api/v1/me` | any | The user of the token. |
| `GET /api/v1/packages` | any | The newest version of each package. |
| `GET /api/v1/packages/{name}` | any | All versions of a package. |
| `GET /api/v1/packages/{name}/{version}` | any | The complete package definition. |
| `GET /api/v1/users` | administrator | All users. |
| `POST /api/v1/users` | administrator | Add a user. |
| `GET /api/v1/tenants` | any | The tenants that you can see. |
| `POST /api/v1/tenants` | administrator | Add a tenant. |
| `GET /api/v1/tenants/{tenant}` | viewer | One tenant and your role. |
| `PUT /api/v1/tenants/{tenant}` | administrator | Change the name and the quota. |
| `DELETE /api/v1/tenants/{tenant}` | administrator | Remove a tenant without deployments. Body: `{"confirm": "<tenant>"}`. |
| `POST /api/v1/tenants/{tenant}/sync` | owner | Prepare the tenant again. |
| `GET /api/v1/tenants/{tenant}/audit` | viewer | The newest 500 audit events. |
| `GET /api/v1/tenants/{tenant}/members` | viewer | The members. |
| `PUT /api/v1/tenants/{tenant}/members` | owner | Add a member or change a role. |
| `DELETE /api/v1/tenants/{tenant}/members/{user_id}` | owner | Remove a member. |
| `GET /api/v1/tenants/{tenant}/environments` | viewer | The environments. |
| `POST /api/v1/tenants/{tenant}/environments` | owner | Add an environment. |
| `DELETE /api/v1/tenants/{tenant}/environments/{env}` | owner | Remove an environment without deployments. |
| `GET /api/v1/tenants/{tenant}/deployments` | viewer | The deployments. |
| `POST /api/v1/tenants/{tenant}/deployments` | developer | Make a deployment. |
| `GET /api/v1/tenants/{tenant}/deployments/{env}/{name}` | viewer | One deployment with all revisions. |
| `PUT /api/v1/tenants/{tenant}/deployments/{env}/{name}` | developer | Make a new revision. |
| `POST /api/v1/tenants/{tenant}/deployments/{env}/{name}/rollback` | developer | Make a new revision that is a copy of an earlier revision. |
| `POST /api/v1/tenants/{tenant}/deployments/{env}/{name}/sync` | developer | Process the desired revision again. |
| `DELETE /api/v1/tenants/{tenant}/deployments/{env}/{name}` | developer | Remove a deployment. |

## Request bodies

`POST /api/v1/users`

```json
{"email": "dev@example.com", "name": "Dev", "password": "at least 12 characters", "admin": false}
```

`POST /api/v1/tenants` (`quota` is optional; the server default applies)

```json
{"slug": "acme", "name": "Acme", "owner": "owner@example.com",
 "environments": ["dev", "prod"], "quota": {"cpu": "4", "memory": "8Gi", "pods": 50}}
```

`PUT /api/v1/tenants/{tenant}`

```json
{"name": "Acme", "quota": {"cpu": "8", "memory": "16Gi", "pods": 100}}
```

`PUT /api/v1/tenants/{tenant}/members`

```json
{"email": "dev@example.com", "role": "developer"}
```

`POST /api/v1/tenants/{tenant}/environments`

```json
{"name": "staging"}
```

`POST /api/v1/tenants/{tenant}/deployments` (`version` is optional; the newest version applies)

```json
{"environment": "dev", "name": "shop", "package": "web-app", "version": "1.0.0",
 "values": {"image": "shop:1.4.2", "replicas": 2, "env": {"LOG_LEVEL": "info"}},
 "message": "First deployment"}
```

`PUT /api/v1/tenants/{tenant}/deployments/{env}/{name}`

Give all values. Values that you do not give get the default of the
package. They do not keep the value of the previous revision.

```json
{"version": "1.0.0", "values": {"image": "shop:1.4.3", "replicas": 2}, "message": "Version 1.4.3"}
```

`POST /api/v1/tenants/{tenant}/deployments/{env}/{name}/rollback`

```json
{"revision": 3}
```

## Example: deploy from a CI pipeline

```bash
curl -fsS -X PUT \
  -H "Authorization: Bearer $SQUARED_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"values": {"image": "shop:'"$GIT_TAG"'", "replicas": 2}, "message": "CI build '"$GIT_TAG"'"}' \
  https://squared.example.com/api/v1/tenants/acme/deployments/prod/shop
```

Then read the deployment until `status` is `deployed`:

```bash
curl -fsS -H "Authorization: Bearer $SQUARED_TOKEN" \
  https://squared.example.com/api/v1/tenants/acme/deployments/prod/shop | jq -r .status
```
