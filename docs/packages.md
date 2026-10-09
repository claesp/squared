# Packages

A package is a tested, versioned definition of an application type. A
platform administrator writes packages. Tenants select a package and give
values for its parameters. This document tells administrators how to write
a package.

## Where packages are

Each package version is one JSON file in the catalog directory. The
repository has the default catalog in [`catalog/`](../catalog). The image
of Squared contains it at `/etc/squared/catalog`. To use a different
catalog, mount a directory and set `SQUARED_CATALOG_DIR`.

Check a catalog before you use it:

```bash
squared check-catalog catalog
```

The server does not start with a catalog that is not valid.

## Rules for versions

- The version has the form `MAJOR.MINOR.PATCH`.
- Never change a published version. Add a new file with a new version.
- Keep old versions in the catalog. Each revision of a deployment also
  keeps a copy of its package, thus a rollback works also without the old
  file. But users can select only versions that are in the catalog.

## Format

```json
{
  "name": "web-app",
  "version": "1.0.0",
  "title": "Web application",
  "description": "An HTTP service with a stable address in the namespace.",
  "parameters": [
    {"name": "image", "title": "Image", "type": "image", "required": true, "example": "app:1.0.0"},
    {"name": "replicas", "title": "Replicas", "type": "integer", "default": 2, "min": 0, "max": 20}
  ],
  "components": [
    {"name": "web", "type": "web", "settings": {"image": "${image}", "replicas": "${replicas}", "port": 8080}}
  ]
}
```

| Field | Rule |
|---|---|
| `name` | Lowercase letters, digits and `-`. At most 40 characters. |
| `version` | `MAJOR.MINOR.PATCH`. |
| `title`, `description` | Text for the web interface. `title` is necessary. |
| `parameters` | The inputs. See below. |
| `components` | At least one component. See below. |

Squared rejects fields that it does not know. This finds typing errors.

### Parameters

| Field | Rule |
|---|---|
| `name` | Lowercase letters, digits and `_`. Starts with a letter. |
| `title` | Label of the form field. Necessary. |
| `description` | Help text below the form field. |
| `type` | `string`, `integer`, `boolean`, `image` or `env`. |
| `required` | The user must give a value. A required parameter has no default. |
| `default` | The value if the user gives no value. |
| `example` | Text in the empty form field. A required `string` parameter must have an example, because Squared uses it to check the package. |
| `enum` | Only for `string`. The list of permitted values. The form shows a list. |
| `pattern` | Only for `string`. A regular expression that the complete value must match. |
| `min`, `max` | Only for `integer`. |
| `project` | Only for `image`. A Harbor project for the image, instead of the project of the tenant. Use a public project, for example `platform`. |

Types:

| Type | Value |
|---|---|
| `string` | Text, at most 1000 characters. |
| `integer` | A whole number. |
| `boolean` | `true` or `false`. The form shows a check box. |
| `image` | `name:tag` or `name@sha256:<digest>`, without registry and project. |
| `env` | Environment variables. The form shows a text field with one `NAME=value` on each line. The API takes an object. |

An empty value means "no value". Squared then uses the default. If there
is no default, the setting that refers to the parameter is not set.

### Components

| Field | Rule |
|---|---|
| `name` | Lowercase letters and digits, no `-`. At most 20 characters. The object names are `<deployment>-<component>`. |
| `type` | `web`, `worker` or `cron`. |
| `settings` | The settings of the component type. A string `"${name}"` takes the value of the parameter `name`. Other values are fixed. |

The `image` setting must refer to a parameter of type `image`. This makes
sure that all images come from the registry and have a digest.

## Component types

| Setting | `web` | `worker` | `cron` | Default | Meaning |
|---|:---:|:---:|:---:|---|---|
| `image` | yes | yes | yes | (necessary) | The image. |
| `replicas` | yes | yes | | 1 | Number of pods, 0 to 50. |
| `command` | yes | yes | yes | image default | List of strings. Replaces the entry point. |
| `args` | yes | yes | yes | image default | List of strings. |
| `env` | yes | yes | yes | none | Environment variables. Names that start with `SQUARED_` are reserved. |
| `cpu` | yes | yes | yes | `100m` | Reserved CPU. |
| `memory` | yes | yes | yes | `128Mi` | Reserved memory and the memory limit. |
| `run_as_user` | yes | yes | yes | image default | User ID. Necessary if the image has no numeric user. |
| `tmp_size` | yes | yes | yes | `64Mi` | Size limit of the writable `/tmp` directory. |
| `port` | yes | | | 8080 | The port of the program, 1024 to 65535. `PORT` has this value. |
| `health_path` | yes | | | `/` | HTTP path for the readiness and liveness checks. |
| `host` | yes | | | none | Public host name. If set, Squared makes an Ingress. |
| `schedule` | | | yes | (necessary) | Cron schedule with five fields. |
| `time_zone` | | | yes | `Etc/UTC` | IANA time zone of the schedule. |

The objects that each type makes:

| Type | Objects |
|---|---|
| `web` | ServiceAccount, Deployment, Service (port 80), Ingress (if `host` is set), PodDisruptionBudget (if more than one replica) |
| `worker` | ServiceAccount, Deployment, PodDisruptionBudget (if more than one replica) |
| `cron` | ServiceAccount, CronJob |

All containers get these variables: `SQUARED_TENANT`, `SQUARED_DEPLOYMENT`
and `SQUARED_COMPONENT`.

## Requirements for images

The renderer uses secure settings that the package cannot change. The
image must agree with them:

- The image runs as a user other than root, with a numeric user ID. If the
  image has a user name, set `run_as_user`.
- The program does not write to the root file system. Only `/tmp` is
  writable.
- The program needs no Linux capabilities. Thus it cannot listen on ports
  below 1024.

## Test a package

The tests render each package of the project catalog and compare the
result with the files in `internal/render/testdata`. After you add or
change a package, add a test case to `internal/render/render_test.go`,
then run:

```bash
go test ./internal/render -update
git diff internal/render/testdata
```

Read the difference carefully before you commit it.
