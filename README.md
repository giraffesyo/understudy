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

```sh
git clone https://github.com/giraffesyo/understudy
cd understudy
make build          # cross-compiles the agents, then embeds + builds the CLI
./bin/understudy version
```

`make build` also creates `ansible` and `ansible-playbook` symlinks — the
binary dispatches on its own name, so those work as drop-in replacements.

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
and vars), `import_tasks`/`import_role` (static) and
`include_tasks`/`include_role` (dynamic: per-host targets, loops, `when`),
handlers with `notify`/`listen`, `block`/`rescue`/`always`, tags, `serial`
rolling batches, `max_fail_percentage`, and `meta` (`flush_handlers`,
`end_play`, `end_host`, `end_batch`, `clear_host_errors`, `clear_facts`,
`reset_connection`, `refresh_inventory`, `noop`).

**Task keywords** — `when`, `loop` / `with_*` (via lookups), `register`,
`until`/`retries`/`delay`, `changed_when`, `failed_when`, `ignore_errors`,
`become`/`become_user`, `vars`, `environment`, `no_log`, `check_mode`,
`delegate_to` (any host) and `delegate_facts`, `run_once`, `any_errors_fatal`,
`connection`/`remote_user`, `action`/`local_action`, `async`/`poll` (including
fire-and-forget).

**Templating** — a Jinja2-compatible engine with `if`/`for`/`set`, ~70
filters (`default`, `combine`, `selectattr`, `regex_replace`, `to_json`,
`map`, `ternary`, `hash`, …), ~50 tests (`version`, `match`, task-result
tests, …), chainable strict `Undefined`, and the native-types rule for
`when:`/`loop:`.

**Modules** — ~30 target-side plus control-side actions, covering the common
system-administration surface:

| Area        | Modules |
|-------------|---------|
| Commands    | `command`, `shell`, `raw`, `script` |
| Files       | `copy`, `template`, `file`, `stat`, `lineinfile`, `blockinfile`, `get_url`, `find`, `tempfile` |
| Packages    | `package`, `apt`, `yum`, `dnf`, `apk`, `pip` |
| Services    | `service`, `systemd` |
| Users       | `user`, `group` |
| Storage     | `mount`, `parted`, `filesystem` |
| Network/sec | `firewalld`, `iptables`, `selinux`, `sysctl`, `modprobe` |
| Config      | `yum_repository`, `sudoers`, `openssh_keypair` |
| Databases   | `mysql_db`, `mysql_user` |
| Facts/util  | `setup`, `ping`, `debug`, `set_fact`, `assert`, `fail`, `meta` |

All modules are idempotent (query-before-mutate) and honor `--check` and,
where meaningful, `--diff`.

**Connections** — SSH (ssh-agent → key files → password auth chain,
`known_hosts` verification, connection reuse) and `local`. `become` via
`sudo`.

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
- **Not yet implemented** (these error clearly, they are not silently
  ignored): `strategy` other than linear, `vars_prompt`, template macros /
  `include`/`extends`, and
  lookups beyond `env`/`file`/`fileglob`/`first_found`/`dict`/`password`.
- **Documented divergences**: YAML timestamps and sexagesimals resolve as
  strings; regular expressions use Go's RE2 (lookaround and backreferences
  in *patterns* are rejected with a clear error rather than mis-matched).

## Building & testing

```sh
make build        # agents + control binary
make test         # unit + local end-to-end tests
make test-e2e     # SSH/container end-to-end tests (requires Docker)
```

The two-stage build cross-compiles the agents first (`CGO_ENABLED=0`,
stripped), embeds them, then builds the control binary. A dependency check
enforces that the agent imports only the module and protocol packages, so it
stays small.

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
