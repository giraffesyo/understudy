# understudy

An Ansible-compatible automation engine written in Go. Runs your existing
playbooks, inventories, roles, and vaults — from a single static binary,
with **no third-party dependencies** and **no Python required on the control
node or on the targets**.

> An understudy learns another performer's role so they can step in and play
> it identically.

```console
$ understudy playbook -i inventory site.yml

PLAY [Configure web servers] **************************************************

TASK [Gathering Facts] ********************************************************
ok: [web01]

TASK [Install nginx] **********************************************************
changed: [web01]

PLAY RECAP ********************************************************************
web01  : ok=2  changed=1  unreachable=0  failed=0  skipped=0  rescued=0  ignored=0
```

## Why

- **One static binary.** `go build` produces a self-contained control binary
  that embeds the target-side agents. Nothing to `pip install`, no
  interpreter to manage, no dependency drift.
- **Nothing on the target but a POSIX shell.** On first contact understudy
  detects the target's OS/arch, pushes a small static Go agent once (cached,
  checksum-named), and runs modules as `agent < task.json`. Targets need no
  Python.
- **Drop-in where it counts.** Real playbooks run unmodified: YAML 1.1
  parsing that matches PyYAML's quirks, a Jinja2-compatible template engine,
  Ansible variable precedence, roles, includes, handlers, blocks, vault, and
  a byte-faithful default output callback.
- **A first-class Go API.** Define playbooks as native Go structs, render
  them to standard playbook YAML, or run them directly — so understudy is
  usable as a library, not just a CLI.

The only dependencies are the Go standard library and `golang.org/x/*`
(the Go team's own extended modules, for SSH and terminal handling). YAML
parsing, the template engine, and Ansible Vault are all hand-written on the
standard library.

## Install

Prebuilt binaries for Linux and macOS (amd64 and arm64) are attached to each
[GitHub release](https://github.com/giraffesyo/understudy/releases), with a
`checksums.txt` of their SHA-256 sums:

```sh
v=v0.1.0 os=linux arch=amd64    # or darwin / arm64
curl -fsSLO https://github.com/giraffesyo/understudy/releases/download/$v/understudy_${v}_${os}_${arch}.tar.gz
tar xzf understudy_${v}_${os}_${arch}.tar.gz
sudo install understudy_${v}_${os}_${arch}/understudy /usr/local/bin/
```

Or build from source (Go, see `go.mod` for the version):

```sh
git clone https://github.com/giraffesyo/understudy
cd understudy
make build          # cross-compiles the agents, then embeds + builds the CLI
./bin/understudy version
```

`make build` also creates `ansible` and `ansible-playbook` symlinks — the
binary dispatches on its own name, so symlinks with those names (to a
release binary too) work as drop-in replacements.

## Quick start

```sh
# Ad-hoc
understudy adhoc all -i 'web01,web02,' -m ping
understudy adhoc all -i inventory -m shell -a 'uptime' --become

# A playbook
understudy playbook -i inventory site.yml --check --diff

# Against localhost with no inventory file
understudy playbook -i 'localhost,' -c local site.yml
```

Inventory works the way you expect — INI and YAML formats, host ranges
(`web[01:20].example.com`), `group_vars/`, `host_vars/`, patterns
(`web:&staging:!db`), and `--limit`.

## Playbooks in Go

The same engine that runs YAML playbooks runs Go structs — define a playbook
natively, then render it to YAML **or** execute it in-process:

```go
package main

import (
	"context"

	understudy "github.com/giraffesyo/understudy"
)

func main() {
	pb := understudy.Playbook{{
		Name:   "Configure web servers",
		Hosts:  "web",
		Become: true,
		Tasks: []understudy.Task{
			{Name: "install nginx", Action: understudy.Package{Name: "nginx"}},
			{
				Name:   "render config",
				Action: understudy.Template{Src: "nginx.conf.j2", Dest: "/etc/nginx/nginx.conf", Mode: "0644"},
				Notify: []string{"restart nginx"},
			},
		},
		Handlers: []understudy.Task{
			{Name: "restart nginx", Action: understudy.Service{Name: "nginx", State: "restarted"}},
		},
	}}

	// Render to a standard playbook file...
	yamlBytes, _ := pb.YAML()
	_ = yamlBytes

	// ...or run it directly.
	result, _ := understudy.Run(context.Background(), pb, understudy.Options{
		Inventory: []string{"inventory"},
	})
	if result.Failed() {
		// inspect result.Hosts["web01"].Failed, etc.
	}
}
```

Typed actions (`Copy`, `Template`, `Service`, `Package`, `User`,
`LineInFile`, `Command`, `Shell`, `Debug`, `Assert`, …) cover the common
modules; `understudy.M{Module: "...", Args: ...}` is the escape hatch for any
other module. `Task.Block`/`Rescue`/`Always` express error handling. Rendered
output is valid `ansible-playbook` input — the render and execution paths go
through the same loader, so they can't disagree.

## Callback plugins

Callbacks are configured exactly as in Ansible (`stdout_callback`,
`callbacks_enabled`, `callback_plugins` / `ANSIBLE_CALLBACK_PLUGINS`, and a
playbook-adjacent `callback_plugins/` directory). Built in: `default`,
`minimal`, `timer`, `profile_tasks`.

With no Python, a plugin is **any executable** named after the plugin in a
callback plugin directory. understudy starts it once per run and writes
every event as one JSON object per line to its stdin, using ansible-core's
hook names:

```json
{"event":"v2_playbook_on_start","version":1}
{"event":"v2_playbook_on_task_start","task":{"name":"install nginx","action":"package","role":"web","path":"roles/web/tasks/main.yml:3"}}
{"event":"v2_runner_on_ok","host":"web01","task":{...},"result":{"changed":true,...}}
{"event":"v2_runner_on_failed","host":"web01","task":{...},"result":{...},"ignore_errors":true}
{"event":"v2_playbook_on_stats","stats":{"web01":{"ok":2,"changed":1,"failures":0,...}}}
```

Other events: `v2_playbook_on_play_start`, `v2_playbook_on_handler_task_start`,
`v2_runner_on_skipped`, `v2_runner_on_unreachable`,
`v2_runner_item_on_{ok,failed,skipped}`, `v2_playbook_on_include`,
`v2_runner_retry`, `v2_runner_on_async_{poll,ok,failed}`. `result` is the
task result as `register:` would capture it. Stdin closes after
`v2_playbook_on_stats`. As the `stdout_callback`, the plugin's stdout is
the run's output; as an enabled callback, its stdout goes to stderr. A
Python-only plugin (`name.py`) cannot load and is reported with Ansible's
`Skipping callback plugin ..., unable to load` warning.

From Go, `understudy.Options.OnEvent` receives the same events.

## Opt-in speedups

These extensions keep playbooks valid for `ansible-playbook`, which simply
ignores them and runs everything as usual.

**Parallel blocks.** A block whose `vars` set `understudy_parallel: true`
runs its direct tasks at the same time (a nested block is one unit and runs
in order). Execution continues once all of them finish. Use it only for
independent work, because siblings keep running when one fails; the block's
`rescue`/`always` still see the failure afterwards.
Package tasks understudy runs are serialized host-wide (dnf and apt only
lock the final transaction, not their download cache), but a shell script
that calls `dnf`/`apt` itself is not covered: don't run two such things in
the same parallel block.

```yaml
- name: build and install independently
  vars: {understudy_parallel: true}
  block:
    - include_role: {name: slurm}
    - include_role: {name: gpu}
```

For portable parallelism that also works under ansible-playbook, `async` +
`async_status` are fully supported.

## Ansible Vault

understudy reads and writes Ansible Vault 1.1 payloads (AES-256-CTR with
HMAC-SHA256 and PBKDF2), fully interoperable with `ansible-vault`:

```sh
# Encrypted vars decrypt transparently at run time
understudy playbook -i inventory site.yml --vault-password-file .vault-pass

# Manage secrets
understudy vault encrypt_string 's3cret' --name db_password --vault-password-file .vault-pass
understudy vault view group_vars/all/vault.yml --vault-password-file .vault-pass
```

`!vault` inline values and whole-file-encrypted `vars_files` /
`group_vars` / `host_vars` all decrypt automatically. From the Go API, set
`Options.VaultPasswords`.

## What's supported

**Language & structure** — plays, roles (with `meta` dependencies, defaults,
and vars, argument-spec validation, `roles_path`), `import_tasks`/`import_role` (static) and
`include_tasks`/`include_role` (dynamic: per-host targets, loops, `when`),
handlers with `notify`/`listen`, `block`/`rescue`/`always`, tags, `vars_prompt`, the `linear`, `free`,
`host_pinned` and `debug` strategies (with the task debugger and the
`debugger` keyword), `serial`
rolling batches, `max_fail_percentage`, and `meta` (`flush_handlers`,
`end_play`, `end_host`, `end_batch`, `clear_host_errors`, `clear_facts`,
`reset_connection`, `refresh_inventory`, `noop`).

**Task keywords** — `when`, `loop` / `with_*` (`items`, `nested`, `together`, `subelements`,
`sequence`, `dict`, `indexed_items`, `flattened`, `lines`, `fileglob`, ...), `register`,
`until`/`retries`/`delay`, `changed_when`, `failed_when`, `ignore_errors`,
`become`/`become_user`/`become_method`/`become_flags`/`become_exe`, `vars`,
`environment`, `no_log`, `check_mode` and `diff` (also inherited from
blocks, role entries and plays),
`delegate_to` (any host) and `delegate_facts`, `run_once`, `any_errors_fatal`,
`connection`/`remote_user`, `action`/`local_action`, `async`/`poll` (including
fire-and-forget).

**Templating** — a Jinja2-compatible engine with `if`/`for`/`set`, macros and
`call`, `include`/`import`/`extends`, block `set`/`filter`/`with`, recursive
loops and `namespace()`, ~70
filters (`default`, `combine`, `selectattr`, `regex_replace`, `to_json`,
`map`, `ternary`, `hash`, …), ~50 tests (`version`, `match`, task-result
tests, …), chainable strict `Undefined`, and the native-types rule for
`when:`/`loop:`.

**Modules** — ~48 target-side plus control-side actions, covering the common
system-administration surface:

| Area        | Modules |
|-------------|---------|
| Commands    | `command`, `shell`, `raw`, `script` |
| Files       | `copy`, `template`, `file`, `stat`, `lineinfile`, `blockinfile`, `ini_file`, `get_url`, `unarchive`, `find`, `slurp`, `tempfile` |
| Packages    | `package`, `apt`, `yum`, `dnf`, `apk`, `pip`, `package_facts` |
| Repos/keys  | `apt_repository`, `deb822_repository`, `yum_repository`, `rpm_key` |
| Services    | `service`, `systemd`, `service_facts` |
| Users       | `user`, `group`, `authorized_key` |
| Storage     | `mount`, `parted`, `filesystem` |
| Network/sec | `firewalld`, `iptables`, `selinux`, `sysctl`, `modprobe` |
| Config      | `sudoers`, `openssh_keypair`, `timezone`, `hostname`, `alternatives`, `getent`, `cron` |
| Web & VCS   | `uri`, `git` |
| Databases   | `mysql_db`, `mysql_user`, `mysql_query`, `mysql_variables`, `mysql_info` (native MySQL/MariaDB protocol client, no Python driver) |
| Facts/util  | `setup`, `ping`, `debug`, `set_fact`, `assert`, `fail`, `meta`, `include_vars`, `validate_argument_spec` |

All modules are idempotent (query-before-mutate) and honor `--check` and,
where meaningful, `--diff`.

**Connections** — SSH (ssh-agent → key files → password auth chain,
`known_hosts` verification, connection reuse, bastions via `ProxyJump` /
`ProxyCommand` from `ansible_ssh_common_args`) and `local`. `become` via
`sudo`, `su` or `doas` (su and doas answer their password prompt on a
pseudo-terminal), configured by keywords or the `ansible_become_*`
connection variables, over either connection: on `local`, an escalated
task's module runs in a child of the understudy binary started through
the become method (a program embedding the Go API serves as that child
itself). On a macOS controller, `local` runs `user`, `group` and
`hostname` with ansible-core's Darwin implementations (`dscl`,
`dseditgroup`, `scutil`).

## Architecture

understudy splits cleanly into two planes:

- **Language plane** — `internal/yaml` (a hand-written, PyYAML-compatible
  parser with source positions), `internal/template` (the Jinja2-compatible
  engine), and `internal/vars` (layered precedence with lazy, use-time
  resolution).
- **Execution plane** — `internal/inventory`, `internal/playbook`,
  `internal/executor` (linear strategy with `forks` parallelism),
  `internal/connection` (SSH + agent bootstrap), and `internal/modules`
  (compiled into both the agent and, for local runs, the control binary).

The agent binary is built for `linux/amd64` and `linux/arm64`, embedded
gzipped via `go:embed`. On first use it is uploaded to a checksum-named path
and cached; subsequent runs skip the upload. `raw` works over the bare SSH
connection, so it functions before the agent exists and on targets the agent
doesn't support.

Fidelity is anchored by differential testing: the YAML parser and template
engine are checked against real PyYAML and Jinja2; a golden corpus runs
through both `ansible-playbook` and understudy and must produce identical
per-task decisions, recaps, and byte-for-byte stdout (reference:
ansible-core 2.21); and end-to-end module behavior is verified against real
Linux hosts (Ubuntu, Alpine, and a systemd Rocky Linux container) with
idempotence re-runs.

## Compatibility notes & limitations

understudy aims to run real playbooks faithfully, and fails loudly rather
than silently diverging. Known boundaries:

- **Targets**: the agent supports `linux/amd64` and `linux/arm64`. Other
  platforms fall back to the `raw` module.
- **Python plugins**: there is no Python anywhere, so plugins that exist
  only as Python code (custom modules, filters, lookups, callbacks) cannot
  be loaded. The common ones are implemented natively — including the
  `timer` and `profile_tasks` callbacks and the `minimal` stdout callback —
  and anything else is reported (`Skipping callback plugin ..., unable to
  load`, `couldn't resolve module/action ...`), never silently skipped.
- **Output reference**: output is byte-compared against ansible-core 2.21.
  Older ansible-core releases word some messages differently (for example
  2.14's `non-zero return code`); task outcomes are the same.
- **Deprecation warnings**: reading a value ansible-core 2.21 deprecates
  (a registered empty loop's `skipped_reason`, `play_hosts`, `vars`,
  facts injected as top-level variables) prints its `[DEPRECATION
  WARNING]` on stderr with the same origin and de-duplication;
  `deprecation_warnings = False` (or `ANSIBLE_DEPRECATION_WARNINGS`)
  silences them.
- **Task debugger**: the `debug` strategy and `debugger` keyword follow
  ansible-core's debugger session (`p`, `c`, `r`, `q`, `help`); `p`
  evaluates Jinja expressions rather than Python. Edits to `task_vars` or
  `task.args` apply to a redo (ansible-core 2.21 ignores them, and its
  `update_task` crashes); `u` is accepted as a no-op.
- **`dig` lookup**: covers A, AAAA, CNAME, MX, NS, TXT, PTR and SRV (not
  yet byte-compared: Ansible's needs dnspython).
- **Documented divergences**: YAML timestamps and sexagesimals resolve as
  strings; regular expressions use Go's RE2 (lookaround and backreferences
  in *patterns* are rejected with a clear error rather than mis-matched),
  iterated with Python's `re.sub`/`findall` match rules.

## Building & testing

```sh
make build        # agents + control binary
make test         # unit + local end-to-end tests
make test-e2e     # SSH/container end-to-end tests (requires Docker)
make test-golden  # differential tests vs. ansible-playbook (see below)
make cross        # build-check the control binary for every release platform
```

The two-stage build cross-compiles the agents first (`CGO_ENABLED=0`,
stripped), embeds them, then builds the control binary. A dependency check
(`make depcheck`) enforces that the agent imports only the module and
protocol packages, so it stays small.

The golden suite runs every `test/e2e/golden/*.yml` through the installed
`ansible-playbook` and through understudy, comparing per-task statuses and
recaps, and stdout byte for byte at the default verbosity and at `-v`. It
needs the `ansible` package (the corpus uses a few `community.general`
plugins) and `passlib`, e.g. `pip install ansible passlib`, with the
matching `ansible-core` release. Set `ANSIBLE_PYTHON_INTERPRETER` to that
Python (CI does), or the output picks up interpreter-discovery warnings.
With Docker available, `test/e2e/golden/linux/*.yml` (modules that need
root: `user`, `hostname`, `alternatives`) also run at `-v` against fresh
Ubuntu, Alpine and Rocky Linux containers, one per tool, and must match
byte for byte.

CI (`.github/workflows/ci.yml`) runs gofmt, `go vet`, `make depcheck`, the
unit suite on Linux and macOS, the cross-compile check, the golden suite
against the latest ansible-core (pinned in one place: `ANSIBLE_CORE_VERSION`
in the workflow), and the Docker end-to-end suite.

### Releases

`understudy version` reports the version stamped at build time with
`-ldflags -X github.com/giraffesyo/understudy/internal/cli.version=...`.
`make build` stamps `git describe` output; builds without the Makefile fall
back to the module version `go install` records.

```sh
make release VERSION=v1.2.3   # dist/understudy_v1.2.3_<os>_<arch>.tar.gz + dist/checksums.txt
```

builds stripped, static tarballs (binary, LICENSE, README) for linux and
darwin on amd64 and arm64. To publish one, push a tag:

```sh
git tag -a v1.2.3 -m v1.2.3 && git push origin v1.2.3
```

The release workflow (`.github/workflows/release.yml`) tests, runs
`make release` with the tag as the version, and creates the GitHub release
with the tarballs and checksums (a tag with a `-`, such as `v1.3.0-rc.1`, is
marked as a prerelease).

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
