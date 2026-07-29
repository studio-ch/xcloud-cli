# xcloud

Command-line interface for the [Cloud Console](https://cloud.flow.swiss).
A single static binary for managing Xcloud instances, data volumes, images,
networks, security groups and elastic IPs — from a terminal or from CI.

> This repository is a **read-only mirror**, published from the Cloud
> Console monorepo on every release. Issues and feature requests are
> welcome here; pull requests are not, since changes here are overwritten
> by the next mirror — see [CONTRIBUTING.md](./CONTRIBUTING.md).

## Install

**Homebrew**

```bash
brew install studio-ch/tap/xcloud
```

**Shell installer** (Linux and macOS, verifies the checksum)

```bash
curl -fsSL https://raw.githubusercontent.com/studio-ch/xcloud-cli/main/install.sh | sh
```

Pin a version, or choose where it lands:

```bash
XCLOUD_VERSION=v0.1.0 XCLOUD_INSTALL_DIR="$HOME/.local/bin" \
  sh -c "$(curl -fsSL https://raw.githubusercontent.com/studio-ch/xcloud-cli/main/install.sh)"
```

**Go**

```bash
go install github.com/studio-ch/xcloud-cli/cmd/xcloud@latest
```

**Manual** — download an archive from
[Releases](https://github.com/studio-ch/xcloud-cli/releases), verify it
against `SHA256SUMS`, and put `xcloud` on your `PATH`. Windows builds are
published as `.zip`.

```bash
xcloud version
```

## Authenticate

Issue an API key in the panel under **Settings → API keys**. The secret is
shown once, at creation. Choose the **Read + Write** preset if you intend
to change anything — a read-only key is rejected on mutations.

```bash
xcloud auth login     # prompts, without echoing
xcloud auth status    # organisation, key, scopes, expiry
```

In CI, set the token in the environment instead:

```bash
export XCLOUD_API_TOKEN=sk_live_…
```

A key belongs to exactly one organisation. For several, use one profile
per key (`xcloud auth login --profile acme`, then `--profile acme` or
`xcloud config use acme`).

## Use

```bash
xcloud region list
xcloud instance list
xcloud instance list -o json | jq -r '.[].name'

xcloud instance create \
  --name build-01 --region ZRH1 \
  --image ghcr.io/example/macos-sequoia:latest \
  --cpu 10 --memory 28 --disk 480 --wait

xcloud instance suspend <id> --wait      # suspend to disk
xcloud instance boot-mode <id> --recovery --wait
xcloud instance delete <id> --yes --wait

xcloud volume create --name scratch --region ZRH1 --size 500
xcloud volume attach <volume-id> --instance <instance-id>
```

Most instance operations are asynchronous. `--wait` blocks until the
instance reaches its new state — use it whenever you script two operations
in a row, because a second action while one is pending is rejected.

`xcloud --help` lists everything; `xcloud <command> --help` goes deeper.

## Scripting

`--output json` emits the API's response body **unchanged**, with only the
list envelope unwrapped. Field names and types are exactly what the API
returns, so `jq` recipes transfer between `curl` and `xcloud`. Table output
carries no such promise — do not parse it.

Progress and warnings go to stderr, so this is safe:

```bash
id=$(xcloud instance create … --wait -o json | jq -r .id)
```

Exit codes are stable; `xcloud exit-codes` prints the full table. The ones
worth branching on:

| Code | Meaning |
|---:|---|
| 0 | success |
| 3 | authentication failed |
| 4 | permission denied (read-only key, or a disabled service) |
| 5 | not found |
| 6 | the resource is busy with another operation |
| 7 | quota exceeded |
| 12 | `--wait` timed out (the operation may still finish) |

### GitHub Actions

```yaml
- name: Provision a build VM
  env:
    XCLOUD_API_TOKEN: ${{ secrets.XCLOUD_API_TOKEN }}
  run: |
    curl -fsSL https://raw.githubusercontent.com/studio-ch/xcloud-cli/main/install.sh | sh
    id=$(xcloud instance create \
      --name "ci-${GITHUB_RUN_ID}" --region ZRH1 \
      --image ghcr.io/example/macos-sequoia:latest \
      --cpu 10 --memory 28 --disk 480 \
      --wait -o json | jq -r .id)
    echo "INSTANCE_ID=$id" >> "$GITHUB_ENV"

- name: Tear down
  if: always()
  env:
    XCLOUD_API_TOKEN: ${{ secrets.XCLOUD_API_TOKEN }}
  run: xcloud instance delete "$INSTANCE_ID" --yes --wait
```

`--yes` is required for destructive commands without a terminal — the CLI
refuses rather than guessing.

## Configuration

`~/.config/xcloud/config.yaml`, created with `0600` permissions because it
holds a token; the CLI refuses to read it if the permissions are looser.

Settings resolve **per field**: flag, then environment
(`XCLOUD_API_URL`, `XCLOUD_API_TOKEN`, `XCLOUD_OUTPUT`, `XCLOUD_PROFILE`),
then the profile, then the default. So a token from the environment
combines with a URL from the profile — the usual CI arrangement.
`xcloud config explain` shows what was resolved and from where.

To keep the secret out of the file, use a command instead:

```yaml
profiles:
  prod:
    api_url: https://api.cloud.flow.swiss
    token_command: op read op://Private/cloud/api-key
```

## Troubleshooting

`--debug` traces every request to stderr with the credential redacted to
its public prefix. Every error carries a `request-id` — quote it to
support.

## Related

- **REST API** — the CLI is a client of the documented public API; anything
  it does can be done with `curl`.
- **MCP server** — connect an AI client to your account with the same key.
- **Packer plugin** —
  [studio-ch/packer-plugin-xcloud](https://github.com/studio-ch/packer-plugin-xcloud)
  bakes custom macOS images.

## Not covered

The CLI authenticates with an API key, and some parts of the panel are not
reachable that way: the VM console and terminal, the AI assistant, the
admin area, and issuing or revoking API keys (listing works; issuing is a
panel operation).
