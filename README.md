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

Inventory works the way you expect — INI, YAML and TOML formats, host
ranges (`web[01:20].example.com`), `group_vars/`, `host_vars/`, patterns
(`web:&staging:!db`), and `--limit`. Sources go through ansible-core's
plugin chain (`host_list`, `script`, `auto`, `yaml`, `ini`, `toml`, and
with `enable_plugins` also `advanced_host_list`, `constructed` and
`generator`): INI values are Python literals as ansible-core reads them
(`yes` stays a string), a YAML file naming a `plugin:` configures that
plugin (`constructed`'s `compose`, `groups` and `keyed_groups`,
`generator`'s `layers`), a source no plugin can parse is reported with
each plugin's failure and skipped, and with nothing parsed only the
implicit localhost remains (which `all` does not match).

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
`group_vars` / `host_vars` all decrypt automatically; a value no password
opens fails where it is used, as ansible-core's undecryptable variable. From
the Go API, set `Options.VaultPasswords`.

## What's supported

**Language & structure** — plays, roles (with `meta` dependencies — for
`include_role`/`import_role` too — `allow_duplicates` and the play's role
cache, defaults, and vars, private or `public`, argument-spec validation,
`roles_path`), `import_tasks`/`import_role` (static) and
`include_tasks`/`include_role` (dynamic: per-host targets, loops, `when`, `apply`),
handlers with `notify`/`listen`, `block`/`rescue`/`always`, tags, `vars_prompt`, the `linear`, `free`,
`host_pinned` and `debug` strategies (with the task debugger and the
`debugger` keyword), `serial`
rolling batches, `max_fail_percentage`, and `meta` (`flush_handlers`,
`end_play`, `end_host`, `end_batch`, `clear_host_errors`, `clear_facts`,
`reset_connection`, `refresh_inventory`, `noop`).

**Fact caching** — ansible-core's builtin cache plugins, `memory` and
`jsonfile` (`fact_caching`, `fact_caching_connection`,
`fact_caching_prefix`, `fact_caching_timeout` and their
`ANSIBLE_CACHE_PLUGIN*` variables), with the `gathering` policy
(`implicit`, `explicit`, `smart`), `set_fact`'s `cacheable`, `meta:
clear_facts` and `constructed` reading the cache. `jsonfile` writes the
files ansible-core does — the schema-qualified name (`<prefix>s1_<host>`),
the payload with each value's tags (where a `set_fact` value or the
template that made it came from), mode 0644 — so `ansible-playbook` and
understudy can share a cache directory, and reads theirs back, origins
included. Two orders cannot be ansible-core's: a gathered fact set's (its
collectors resolve through Python sets of names, whose string hashes
are randomized per process, so ansible-core's own order varies between
runs; understudy writes them by name), and a mapping's keys within a
module's facts (by name). A cache plugin from a collection (`redis`,
`yaml`, ...) is Python and cannot load: it warns as ansible-core does
and the memory cache stands in.

**Task keywords** — `when`, `loop` / `with_*` (`items`, `nested`, `together`, `subelements`,
`sequence`, `dict`, `indexed_items`, `flattened`, `lines`, `fileglob`, ...) with
`loop_control` (`loop_var`, `index_var`, `label`, `extended`, `pause`,
`break_when`), `register`,
`until`/`retries`/`delay`, `changed_when`, `failed_when`, `ignore_errors` (also
inherited from blocks and plays, the nearest setting winning),
`become`/`become_user`/`become_method`/`become_flags`/`become_exe`, `vars`,
`environment` (mappings, templates or a list of them, merged play, role,
block, task), `timeout`, `no_log`, `check_mode` and `diff` (also inherited from
blocks, role entries and plays),
`delegate_to` (any host) and `delegate_facts`, `run_once`, `any_errors_fatal`,
`connection`/`remote_user`, `action`/`local_action`, `async`/`poll` (including
fire-and-forget).

**Templating** — a Jinja2-compatible engine with `if`/`for`/`set`, macros and
`call`, `include`/`import`/`extends`, block `set`/`filter`/`with`, recursive
loops and `namespace()`, in-place list/dict methods (`append`, `update`, ...), all
122 builtin filters (`default`, `combine`, `selectattr`, `regex_replace`,
`to_json`, `map`, `urlize`, `to_datetime`, `password_hash`, …) and 88 tests
(`version`, `match`, task-result tests, `url`, …), chainable strict
`Undefined`, and the native-types rule for
`when:`/`loop:`. Templates compile through a port of Jinja2's lexer and
parser, so a broken template fails with Jinja's own `TemplateSyntaxError`
(`expected token 'end of print statement', got 'integer'`, `Encountered
unknown tag 'do'.`, line numbers in template files); filter and test
failures read as ansible-core reports the Python exception its plugin
raised (`The filter plugin 'ansible.builtin.combine' failed: ...`), and
conditionals, loops and template files fail with ansible-core's error
chains. Ints are arbitrary precision, as Python's are.

**Modules** — ~48 target-side plus control-side actions, covering the common
system-administration surface:

| Area        | Modules |
|-------------|---------|
| Commands    | `command`, `shell`, `raw`, `script` |
| Files       | `copy`, `template`, `file`, `stat`, `lineinfile`, `blockinfile`, `ini_file`, `get_url`, `unarchive`, `find`, `slurp`, `tempfile` |
| Packages    | `package`, `apt`, `yum`, `dnf`, `dnf5`, `apk`, `pip`, `package_facts` |
| Repos/keys  | `apt_repository`, `deb822_repository`, `yum_repository`, `rpm_key` |
| Services    | `service`, `systemd`, `service_facts` |
| Users       | `user`, `group`, `authorized_key` |
| Storage     | `mount`, `parted`, `filesystem` |
| Network/sec | `firewalld`, `iptables`, `selinux`, `sysctl`, `modprobe` |
| Config      | `sudoers`, `openssh_keypair`, `timezone`, `hostname`, `alternatives`, `getent`, `cron` |
| Web & VCS   | `uri`, `git` |
| Databases   | `mysql_db`, `mysql_user`, `mysql_query`, `mysql_variables`, `mysql_info` (native MySQL/MariaDB protocol client, no Python driver) |
| Facts/util  | `setup`, `ping`, `debug`, `set_fact`, `set_stats`, `assert`, `fail`, `meta`, `include_vars`, `validate_argument_spec` |
| Inventory   | `add_host`, `group_by` |

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
`dseditgroup`, `scutil`). Temporary files follow ansible's rules under
become, on either connection: a transfer is staged in the login user's
`remote_tmp` (and reported there), and a become user that is neither an
`admin_users` member nor the login user gets a directory in a
`system_tmpdirs` dir, made readable to it by `_fixup_perms2`'s chain
(setfacl, chown, `chmod +a`, `common_remote_group`,
`allow_world_readable_tmpfiles`) with ansible's warnings and errors. The
login user stages a transferred file there, so where the module may
write the directory (a common group's `g+rwx`) `copy` renames that file
into place and then cannot `chmod` it, failing as ansible's does. Such a
module gets no temporary directory of its own: it makes one under
`remote_tmp` when it needs one (an unreadable working directory, a
`lineinfile`/`replace`/`uri`/`get_url` temp file), creating `remote_tmp`
with ansible's "Module remote_tmp ... did not exist" warning. A become
user wrongly listed in `admin_users` cannot read the module staged in
the login user's `remote_tmp`, and the task fails with ansible's
"Module result deserialization failed" and Python's output. Pipelining
is not modeled: temporary files behave as with ansible's default
(`pipelining = False`).

## Architecture

understudy splits cleanly into two planes:

- **Language plane** — `internal/yaml` (a port of libyaml, the parser
  ansible-core loads YAML with: same errors and positions, PyYAML-compatible
  construction, source positions; and of its emitter, which `to_yaml` and
  `to_nice_yaml` dump through as ansible-core's do, aliases for values
  referenced twice included), `internal/template` (the Jinja2-compatible
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
  (a registered empty loop's `skipped_reason`, a timed-out task's
  `timedout.frame`, `play_hosts`, `vars`, facts injected as top-level
  variables) prints its `[DEPRECATION WARNING]` on stderr with the same
  origin (a variable's definition, a list entry, `<<container>>`) and
  de-duplication; filters warn only for the values they read.
  `deprecation_warnings = False` (or `ANSIBLE_DEPRECATION_WARNINGS`)
  silences them.
- **Inventory plugins**: ansible-core's own inventory plugins are native
  (`host_list`, `advanced_host_list`, `script`, `auto`, `yaml`, `ini`,
  `toml` with a port of Python's `tomllib`, `constructed`, `generator`),
  their option validation and errors included; a config naming a
  collection's plugin fails to parse as an unknown plugin, and the source
  is skipped as ansible-core skips one it cannot parse. TOML dates and
  times load as their `isoformat()` strings (as YAML timestamps load as
  strings). ansible-core orders a group's hosts two or more levels of
  child groups down by Python set iteration (object addresses); understudy
  takes them level by level in the order the groups were added, so such
  hosts can list in another order in `groups`. `constructed`'s
  `use_vars_plugins` reads `group_vars/` and `host_vars/` next to the
  sources parsed before it, its options are read from the plugin's
  config, the environment and `ansible.cfg` (`[inventory_plugins]
  use_extra_vars`) as ansible-core reads them, and hosts' cached facts
  join their variables (a persistent cache's, as ansible-core opens a
  new cache plugin for it). `meta:
  refresh_inventory` parses the sources again and replaces the hosts,
  groups and variables, then makes the run's `add_host` and `group_by`
  changes again (hosts first, then groups; a host the refresh dropped
  loses its `group_by` groups): a play host the refreshed inventory
  dropped runs nothing more in the play, and a new one runs from the next
  play.
- **`add_host` and `group_by`**: both change the run's in-memory
  inventory at once, as ansible-core 2.21's inventory RPC does, with its
  `changed` rules (a host, group, membership or variable that really
  changed), its results and errors. `add_host` bypasses the host loop:
  the linear strategy runs it on the first host only (once per loop
  item), its result, register and failure stay that host's, and a
  failure fails every host left in the batch, as a `run_once` task's
  does; the free strategies refuse it, ending the run. A host it adds
  joins `ansible_play_hosts(_all)` for the rest of the batch. New groups
  and hosts take their `group_vars/` and `host_vars/` files, which stay
  over the variables `add_host` gives. `TRANSFORM_INVALID_GROUP_CHARS`
  applies to the groups the two create (a parent named with invalid
  characters then fails as in ansible-core) and to those every inventory
  plugin adds, with its warnings: a child an INI `:children` section or
  a TOML `children` list names as written then is not found, as in
  ansible-core. Host patterns
  are cached until the inventory changes, so a host left half-made by a
  failed `add_host` is not matched until the next change, as in
  ansible-core.
- **Connection variables**: a task's connection settings join its
  variables under the names it does not set, as ansible-core's
  PlayContext and connection plugin add them: every name of each setting
  (`ansible_port` and `ansible_ssh_port`, `ansible_host` and
  `ansible_ssh_host`, ...) for `when` and the task's arguments, the
  plugin's own names for `debug`'s `var`, `changed_when`, `failed_when`
  and `until`; a delegated task takes the delegated host's. Neither
  `vars` nor `hostvars` lists them, and a delegated result's label names
  the host's address (`h1 -> d1(192.0.2.1)`) where its connection plugin
  resolves one other than its name.
- **Command line and configuration**: `ansible-playbook` and `ansible`
  parse their options as ansible-core's argparse does (abbreviations,
  combined short options, mutually exclusive options), with the same
  usage, errors, help text and exit code 2. `ansible.cfg` is read as
  Python's `configparser` reads it and its typed settings are checked as
  ansible-core's constants load; a bad file or value ends the command
  with `ERROR: <message>` and exit code 5, but without the Python
  traceback ansible-core prints after the message. `--version` names
  understudy in place of ansible-core's Python details.
- **Exit codes**: as ansible-playbook, the result of the last play run
  (failed and unreachable hosts carry over between plays until
  `clear_host_errors`); several playbooks each end with a recap, and one
  that fails is the last run.
- **Environment order**: the task's variables lead the module's
  environment in ansible-core's merge order; where a module runs through
  `/bin/sh` (`shell`), the shell reorders them as it does under
  ansible-core.
- **Task debugger**: the `debug` strategy and `debugger` keyword follow
  ansible-core's debugger session (`p`, `c`, `r`, `u`, `q`, `help`):
  `task.args` edits apply to a redo, `task_vars` edits through
  `update_task`, which loads the task again templated with them (losing
  `task.args` edits), as in ansible-core. With no Python, `p` evaluates
  Jinja expressions and a statement can only be an assignment to (or
  `del` of) `task.args[...]` or `task_vars[...]`, not arbitrary Python.
  A redo after `update_task` on a task with `register` ends the run as
  ansible-core 2.21's crashing worker does (`A worker was found in a dead
  state`, exit 1), without the Python traceback it prints.
- **`dig` lookup**: covers A, AAAA, CNAME, MX, NS, TXT, PTR and SRV (not
  yet byte-compared: Ansible's needs dnspython).
- **Output that depends on the target's Python**: understudy never runs
  Python, but some of ansible's output comes from the Python that runs its
  modules. understudy identifies that interpreter as ansible would (the
  task's `ansible_python_interpreter`, else `ANSIBLE_PYTHON_INTERPRETER`,
  else discovery's `python3.14` ... `python3.9`, `/usr/bin/python3`
  order, or `ansible_interpreter_python_fallback`, searched in the login
  user's PATH; discovery reports the `discovered_interpreter_python` fact
  and its warnings as ansible's does) and names it as that Python reports `sys.executable`
  (macOS's `/usr/bin/python3` shim is the developer directory's python3,
  Homebrew's Pythons their `opt` link) wherever ansible's messages do:
  `missing_required_lib` errors, `pip`'s `python -m pip` command and
  virtualenv `-p`, `dnf`'s probed interpreters. It reads what it needs off
  the filesystem: the version from the
  interpreter's path, installed libraries from the `.dist-info`/
  `.egg-info` metadata in its site directories (venv `pyvenv.cfg` and
  `.pth` files included). The rule: where the target has a Python,
  modules behave as ansible's would with it; where it has none (where
  ansible could not run at all), the native implementation stands in.
  - `uri`/TLS errors: CPython appends the `_ssl.c` line that raised
    (`... self-signed certificate (_ssl.c:1082)`). That line changes
    between CPython releases and even between distribution builds of one
    release, so understudy adds it only for builds it has measured,
    identified by the interpreter's install path (Homebrew, pyenv, uv) or
    its dpkg/apk/rpm package version (Ubuntu 24.04, Debian 12, Alpine
    3.20, Rocky Linux 9); for any other build the location is left out
    rather than claimed.
  - `uri` with `file://` and `ftp://`: urllib opens them, but their
    responses have no status, so in ansible-core 2.21 a successful one
    crashes the module (`int() argument must be ... not 'NoneType'`, worded
    per Python version) and failures are ordinary URLErrors; understudy
    reproduces both, FileHandler's host checks by Python version included.
    `form-multipart` part types come from the target's `mimetypes`,
    which reads its `mime.types` files (`/etc/mime.types`,
    `/etc/apache2/mime.types` on macOS) over Python's built-in table.
  - `mysql_*`: `connector_name`/`connector_version` (and the connection
    attributes) name the driver ansible's modules would import there:
    PyMySQL, else mysqlclient (MySQLdb, with its deprecation warning). A
    target Python with neither fails with `mysql_driver_fail_msg`, as
    ansible does; a target without Python reports the native client's
    identity, PyMySQL 1.1.2, the release whose behavior it reproduces.
  - `openssh_keypair`: the `cryptography` backend is implemented natively
    (OpenSSH, PKCS1 and PKCS8 keys, passphrases, cryptography's quirks such
    as an encrypted PKCS8 key never matching `private_key_format: pkcs8`).
    `backend: auto` picks as ansible does (ssh-keygen unless a passphrase
    is set); where the target has a Python, the backend is available only
    if ansible's would be (python `cryptography` >= 3.3, and `bcrypt` for
    passphrases, else ansible's errors).
  - `pip`: names are parsed as the `packaging` library the task's Python
    imports parses PEP 508 requirements (or setuptools' `pkg_resources`,
    the module's fallback), and its release picks the rules: the
    pyparsing grammar with `LegacyVersion`/`LegacySpecifier` before 22.0
    (Rocky 9's 20.9), the hand-written parser after, 26.x's `name @ url`
    spacing, quoting and specifier de-duplication. That decides each
    name's text on pip's command line and, in check mode, whether an
    installed version satisfies it (PEP 440 `==`/`!=` wildcards and local
    versions, `~=`, `===`, prereleases); references packaging cannot
    parse are resolved with `pip install --dry-run --report` where pip is
    24.1 or later, as the module does. A virtual environment's Python
    sees only its own site-packages unless it includes the system's.
    The module's crashes on a non-PEP 440 installed version (packaging
    22-25) and on a pip it cannot ask its version are reproduced. Not
    modeled: packaging 22.x's short-lived quirks, and the SyntaxError a
    marker string Python cannot unescape raises before 26.3 (treated as
    an invalid requirement).
  - Errors that quote `sys.version` (`apt` and `dnf5` without their
    bindings): CPython assembles it from strings compiled into the
    interpreter or its libpython (`PY_VERSION`, the build date and time,
    the compiler banner, with its newline on older builds), which
    understudy reads off the ELF or Mach-O binary. Where they cannot be
    found unambiguously (a stripped or non-CPython build) only the
    `major.minor` version is shown.
  - `setup`'s `ansible_python` facts describe the interpreter running
    the module, named by its `sys.executable`; the type is always
    `cpython` (a PyPy target would say `PyPy`).
  - Name lookup failures (`uri`, `get_url`, `mysql_*`) read as Python's
    `socket.gaierror`, worded by the target's C library (glibc, musl on
    Alpine, macOS). The lookup itself is Go's resolver, which applies a
    `resolv.conf` search list as glibc does, not as musl does.
  - Missing package-manager bindings: without python3-apt, `apt` and
    `apt_repository` fail as ansible's do where they cannot install it
    (check mode, `install_python_apt: false`, `auto_install_module_deps:
    false`, the latter quoting the Python's `sys.version`); otherwise
    they install python3-apt with apt-get first, as ansible's modules do
    (apt's warning goes with the process it respawns from, so it is not
    shown). `package_facts` cannot use its apt backend without
    python3-apt. `dnf` fails without the dnf Python package, and `dnf5`
    without libdnf5 first runs `dnf install -y python3-libdnf5` (or,
    with `auto_install_module_deps: false`, fails quoting
    `sys.version`), as ansible's modules do.
- **`dnf` results**: the transaction runs through the dnf CLI, and
  `results` lists it as the modules do (`Installed: <nevra>`,
  `Removed: <nevra>`). dnf4's module iterates a set, so for multi-package
  transactions understudy lists the requested packages first and their
  dependencies after, by name.
- **Verbose output**: `-vv` prints what ansible-core does: the `PLAYBOOK:`
  banner and play count, task paths, handler notifications, `META:` lines,
  skipped stdout callbacks, static imports, plugin redirects
  (`redirecting (type: modules) ...`, at load and each time a task resolves
  one) and the password hashing backend. Its version banner names
  understudy's build rather than ansible-core's Python installation.
  `-v` names the plugin a `plugin:` config runs (`Using inventory plugin
  ...`); at `-vvv` each inventory source shows the plugins that declined
  it and the one that parsed it, results dump indented and the local
  connection announces itself (`<host> ESTABLISH LOCAL CONNECTION FOR USER:
  ...`); the lines that trace ansible-core's Python machinery are not
  reproduced: the `EXEC`/`PUT` commands that stage and run AnsiballZ
  payloads, `Using module file`, the variable manager's repeated ``Read
  `vars_file` `` lines, and SSH connection tracing (understudy's agent
  protocol runs no per-command `ssh`).
- **Documented divergences**: YAML timestamps and sexagesimals resolve as
  strings; regular expressions use
  Go's RE2 (lookaround and backreferences in *patterns* are rejected with a
  clear error rather than mis-matched),
  iterated with Python's `re.sub`/`findall` match rules; a task that hits
  its `timeout` has the processes its module started killed — each command
  a module runs leads its own process group, so its background jobs and
  pipeline stages go with it, on the control node and (the agent being
  terminated in turn) on SSH targets (ansible-core leaves them running,
  even after the playbook exits) — output is identical, only the orphaned
  work is stopped. A Ctrl-C (or SIGTERM/SIGHUP) is passed on to those
  process groups, so it still stops them as it would in the terminal's.
- **Template error details**: filter errors name the Python class of
  their values as ansible-core's plugins see them — lazy containers and
  tagged scalars for variables, plain types for values computed in the
  template — judged from the filter's argument expressions (a registered
  result's scalars are named as tagged too). `to_json` of a value that
  contains itself fails in CPython with a C-stack message that depends on
  the platform (`Stack overflow (used 16354 kB)`); understudy reports
  `maximum recursion depth exceeded`. Filters and tests bind their
  arguments to ansible-core's Python signatures (`takes 1 positional
  argument but 2 were given`, `got an unexpected keyword argument 'x'.
  Did you mean 'y'?`). `union`/`intersect`/`difference`/
  `symmetric_difference` come out in CPython's set order for numbers
  (string hashes are randomized per Python process; understudy keeps
  their first-seen order), and seeded `random`/`shuffle` pick what
  Python's Mersenne Twister picks. Every ansible.builtin filter and
  test (Jinja2's and ansible-core's) is implemented. `password_hash`'s
  `blowfish` (bcrypt) hashes as the controller's ansible-core does:
  through passlib (no libxcrypt: the salt's unused bits repaired,
  passlib's validation and errors) or libxcrypt's `crypt_gensalt` (the
  salt's first 16 bytes encoded). `escape`, `safe`, `forceescape` and
  `tojson` return markup, which stays markup through the case filters and
  `+` (escaping the other operand) and becomes a str, with ansible-core's
  warning, in a template's result; `to_datetime` returns a datetime
  (attributes, `strftime`/`isoformat`/`timestamp`/`weekday`, subtraction
  to a timedelta, comparison; stored as itself, shown as its
  `isoformat()`), and a timedelta in a template's result fails as
  unsupported for variable storage. String literals in `{{ }}` keep their
  backslashes, as ansible-core's `escape_backslashes` has them. Not
  modeled: markup through `format`, slicing and `%`; `rekey_on_member` on
  a member that is not a string keys by its str() (dict keys are
  strings); `fileglob` lists a directory in its own order, as Python's
  `os.scandir` does; a plugin error about a value (`rekey_on_member`'s
  missing key) shows the value with an unknown origin where ansible-core
  knows a variable's; comparing or subtracting a variable's datetime
  names `datetime.datetime` where ansible-core names
  `_AnsibleTaggedDateTime`; `attr` of a method and a bare method in a
  template's result render (an address) rather than failing as
  unsupported for variable storage.
- **Broken conditionals**: a conditional whose result is not a boolean
  fails as ansible-core's broken conditional (or, with
  `ALLOW_BROKEN_CONDITIONALS`, warns), naming where the result's value
  came from: a variable's definition (play, `vars_files`, `include_vars`,
  role, YAML and INI inventory, `-e`), a `set_fact` argument, an item of
  one of those (a loop's item included, and values read through
  `hostvars`), or the expression for a computed or registered value.
- **`hostvars`**: a host's variables through `hostvars` are those
  ansible-core's `get_vars` gives without a play (inventory, facts,
  `include_vars`, `set_fact` and registered values, extra vars and the
  run's magic variables, no play or role variables), listed in the order
  it combines them; `ansible_playbook_python` names the first `python3`
  on `PATH`.
- **`debug var=`**: undefined values render in place as ansible-core's
  placeholders. A method or class (`d.items`, `range`) fails as
  "unsupported for variable storage" there and in any template's result,
  and renders as its Python repr in text; the object addresses some of
  those reprs show (builtin methods, functions) cannot match a Python
  process's.

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
recaps, and stdout byte for byte at the default verbosity, `-v`, `-vv` and
`-vvv` (the version banner masked at `-vv`, and at `-vvv` the lines listed
under **Verbose output** left out). It
needs the `ansible` package (the corpus uses a few `community.general`
plugins) and `passlib`, e.g. `pip install ansible passlib`, with the
matching `ansible-core` release. Set `ANSIBLE_PYTHON_INTERPRETER` to that
Python (CI does): the harnesses pin it so most output does not depend on
which Python discovery finds (`interpreter_discovery.yml` sets
`ansible_python_interpreter: auto` to cover discovery itself).
The pip playbooks run the module under the Python that runs
`ansible-playbook` (it has `packaging`; the harness passes it as
`GOLDEN_PACKAGING_PYTHON`) and install offline into virtualenvs.
With Docker available, `test/e2e/golden/linux/*.yml` (modules that need
root or a real Linux target: `user`, `hostname`, `alternatives`, `dnf`,
package parameters, git over ssh, uri's Python-dependent output, pip
against each distribution's Python and packaging, and on a
booted systemd Rocky container `systemd`, `firewalld` and `selinux`) also
run at `-v` against fresh Ubuntu, Alpine and Rocky Linux containers, one
per tool, discovering each target's Python, and must match byte for byte.
`test/e2e/golden/cli` runs `ansible-playbook` and `ansible` command lines
(through understudy's symlinks) in a fresh directory with the files,
environment and modes each case names, comparing stdout, stderr and the
exit code byte for byte: argparse's usage, errors and help, `ansible.cfg`
discovery and errors (the Python traceback after a configuration error
is left out), and settings only a command line or config reaches.

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
