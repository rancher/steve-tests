# steve-tests

Go tests for standalone Steve: CRUD, filtering, sorting, pagination, and SQL cache behavior.

## Running tests

Requires Go 1.27.2+.

```sh
go test -p=4 -parallel=4 ./...
```

Live tests skip unless a Steve endpoint is configured. Copy [steve-config.yaml.example](steve-config.yaml.example) to `~/steve-config.yaml`, set the endpoint, and run the full suite:

```sh
STEVE_CONFIG="$HOME/steve-config.yaml" \
STEVE_TEST_ALLOW_MUTATIONS=true \
STEVE_TEST_ALLOW_WORKLOADS=true \
STEVE_TEST_ALLOW_CRDS=true \
go test -count=1 -p=4 -parallel=4 -timeout=20m -v ./...
```

Omit the permission flags for read-only checks. Tests clean up their resources. Set `STEVE_TOKEN` if authentication is required; use `STEVE_CA_FILE` for a private CA or `STEVE_INSECURE_SKIP_VERIFY=true` for a local self-signed endpoint. `STEVE_URL` can override the configured endpoint.

Set `STEVE_VERSION` for local runs to use version-specific expectations and skip unsupported features. GitHub Actions sets it automatically.

In GoLand, select [**All Steve tests**](.run/All%20Steve%20tests.run.xml) and click Play. It uses `~/steve-config.yaml`, enables all test permissions, and shows individual results. The [TestAll](all_test.go) wrapper also runs the suite but appears as one test.

## End-to-end runs

Requires Docker, k3d, kubectl, jq, and Go; gotestsum is optional. This command builds the selected Steve tag, creates a disposable k3d cluster, runs the full suite, and tears it down:

```sh
STEVE_VERSION=v0.10.9 scripts/e2e.sh all
```

To keep the environment between runs:

```sh
STEVE_VERSION=v0.10.9 scripts/e2e.sh up
scripts/e2e.sh test
E2E_RUN='TestPods' scripts/e2e.sh test
scripts/e2e.sh down
```

Logs and test results survive teardown. The runner enables and verifies SQL cache mode. Run `scripts/e2e.sh` without arguments for all settings.

## GitHub Actions

| Workflow | Purpose |
| --- | --- |
| [Steve E2E Tests](.github/workflows/steve-tests-e2e.yml) | Manually test a Steve tag on k3d, save results and diagnostics, and optionally report to Qase. |
| [Upload Steve Qase Schemas](.github/workflows/upload-qase-schemas.yml) | Create/update Qase suites and cases before the first reported run and after schema changes. |
| [Check Steve Tags](.github/workflows/check-steve-tags.yml) | Check for new tags daily at 14:00 UTC or manually; dispatch E2E runs with Qase enabled. |
| [Steve Tests CI](.github/workflows/ci.yml) | Check workflows, shell scripts, unit tests, and schema coverage on pull requests or manually. |

For a first run, open **Actions → Steve E2E Tests → Run workflow**, select a branch, and enter a tag such as `v0.10.9`. Leave optional inputs blank for the full suite. Turn off **Report results to Qase** to run without secrets or AWS setup.

Manual workflows must exist on the default branch before their **Run workflow** button is available. No existing Kubernetes cluster is needed.

### Secrets and Qase

Add repository secrets under **Settings → Secrets and variables → Actions**:

| Secret | Value | Used by |
| --- | --- | --- |
| `QASE_AUTOMATION_TOKEN` | API token with access to suites, cases, milestones, runs, and results | Qase upload/reporting |
| `QASE_PROJECT_ID` | Project code: `RM` | Qase upload/reporting |
| `AWS_ROLE_ARN` | IAM role trusted by GitHub OIDC | Tag watcher |
| `AWS_REGION` | Cache bucket's region | Tag watcher |
| `STEVE_TAGS_S3_BUCKET` | Bucket name, without `s3://` | Tag watcher |

After adding the Qase secrets, run **Upload Steve Qase Schemas**, then **Steve E2E Tests** with reporting enabled. Leave the run ID blank to create a new run. Runs close only when tests and reporting both succeed.

Each top-level test under `tests/` maps to one Qase case in [steve_schemas.yaml](schemas/steve_schemas.yaml), under `Go Automation/Steve/<package>`. Subtest counts and failures appear in the parent's result comment; internal harness tests are excluded. Qase totals therefore differ from the Go test total.

The schemas target `RM`; changing projects requires updating their `projects` entries as well as the secret. RM runs require custom field `16` with value `1` (`RecurringRun`). Milestones use the prefix `Steve`.

### Automatic tag discovery

Create a private S3 bucket with S3-managed encryption and an IAM role using [GitHub OIDC](https://docs.github.com/en/actions/how-tos/secure-your-work/security-harden-deployments/oidc-in-aws). Restrict trust to the intended repository and branch, with audience `sts.amazonaws.com`; match the repository's immutable subject format if enabled. Grant:

- `s3:ListBucket` on the bucket.
- `s3:GetObject` and `s3:PutObject` on `steve/*` in that bucket.

Add the three AWS secrets and run **Check Steve Tags**. Repository policies must allow `id-token: write` and `actions: write`. Static AWS keys and a GitHub PAT are unnecessary.

- Tracks the highest Steve tag, including prereleases, for each minor used by the **five newest Rancher release branches plus `main`**. Set the `RANCHER_RELEASE_LINES` repository variable to change the window (default `5`).
- A minor without a cached tag dispatches on the next scan; later scans dispatch only changed tags. Shared Steve minors produce one dispatch. Older tags are not backfilled.
- Always dispatches E2E runs from the repository's **default branch**.
- Stores dispatched tags in `steve/<minor>.txt`; test artifacts stay in GitHub. Keep cache objects out of expiration policies. Failed tests need a manual retry; manual E2E runs do not update the cache.
- Enable scheduled workflows in forks. If a fork and upstream use the same bucket and prefix, schedule discovery in only one.

## Adding tests

- **Indexed field:** add a row to [TestSQLIndexedFields](tests/indexedfields/indexedfields_test.go) with fixtures and an update.
- **Filter, sort, or pagination:** add a resource collection scenario; see [ConfigMaps](tests/configmaps/collection_test.go).
- **Different setup:** add a test in the matching resource package using helpers in `actions/`.
- **New top-level test:** add its case to [steve_schemas.yaml](schemas/steve_schemas.yaml) and upload the schemas.

Run one case with `-run`:

```sh
go test -count=1 -v ./tests/indexedfields -run 'TestSQLIndexedFields/hpa_min_replicas'
```

Keep cases independent, use explicit expected results, and clean up resources. Use `t.Parallel()` only for cases that own their fixtures.
