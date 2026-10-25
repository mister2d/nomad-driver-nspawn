# Fleet additions

This fork adds options for jailed, ephemeral sandbox machines. Each option is
off or neutral by default. Existing jobs keep their behavior.

## Environment names

systemd-nspawn `--setenv` accepts only `[A-Za-z0-9_]` names without a leading digit. Nomad builds
some names from task names, for example `NOMAD_ALLOC_IP_connect-proxy-<service>` when a group has a
Consul Connect sidecar. The driver renames such a variable (each other character becomes `_`, a
leading digit gets a `_` prefix) and skips it with a warning if the new name is already set. Without
this a machine next to a Connect sidecar fails with "Cannot assign environment variable".

## Feature attribute

The fingerprint sets `driver.nspawn.features` to a sorted, comma separated
list: `cpu-hard-limit, cpu-weight, env-names, exec-env, hostname,
load-credentials, portmap-bridge, scope-stats, store-root, tasks-max,
unprivileged-ports, userns-netns`. A job can require a feature:

```hcl
constraint {
  attribute = "${attr.driver.nspawn.features}"
  operator  = "set_contains"
  value     = "tasks-max"
}
```

## Plugin options

```hcl
# The block label is the plugin binary's file name, not the driver name "nspawn" that jobs use.
# With plugin "nspawn" Nomad logs "plugin not referenced in the agent configuration file".
plugin "nomad-driver-nspawn" {
  config {
    cpu_hard_limit    = false  # true: apply CPUQuota to every task
    default_tasks_max = 0      # 0: keep the systemd default. Above 0: default and ceiling
  }
}
```

## Task options

| Option | Type | Effect |
|---|---|---|
| `port_map` | list(map(number)) | In bridge mode the host port comes from `NOMAD_HOST_PORT_<label>`. The driver passes no `--port` when a network namespace is used. Nomad CNI portmap does the forwarding. |
| `cpu_hard_limit` | bool | Sets `CPUQuota` on the machine scope from the task CPU limit. Needs Nomad to send a CPU quota or percent value for this driver. |
| `tasks_max` | number | Sets `TasksMax` on the scope. A lower value wins over `default_tasks_max`. A higher value is reduced to `default_tasks_max` when that option is above 0. 0 uses the default. A negative value fails the start. |
| `load_credentials` | list(map(string)) | `ID = "path"` becomes `--load-credential`. The file must be a regular file in the task `local/` or `secrets/` directory. |
| `set_credentials` | list(map(string)) | `ID = "value"` becomes `--set-credential`. The value is visible in `ps`. IDs with pass, secret, token, key, private or cert are refused. |
| `root_mode` | string | `image` (default) runs the image as the root. `store` runs a NixOS image from its store only. See "Store root mode". |
| `toplevel` | string | Store mode only. The `/nix/store/<name>` toplevel to run. The default follows `/nix/var/nix/profiles/system` inside the image. |
| `root_size` | string | Store mode only. Size of the tmpfs root, for example `256m` (default) or `1g`. |
| `unprivileged_port_start` | number | Sets `net.ipv4.ip_unprivileged_port_start` in the group network namespace before start. -1 (default) leaves it. Needs bridge or cni networking. Use 0 to let sshd bind port 22 from inside a user namespace. |
| `hostname` | string | Becomes `--hostname`. Must be a DNS name of at most 64 characters. An unresolved `${...}` fails validation. Empty (default) keeps the nspawn default, the machine name. See "Hostname". |

## Hostname

Set the hostname from the allocation, for example
`hostname = "sbx-${NOMAD_SHORT_ALLOC_ID}"`. Nomad interpolates the value before
the driver gets it. nspawn sets the name in the machine UTS namespace before
init starts.

systemd in the machine sets the hostname at boot in this order:
`systemd.hostname=` on the init command line, then `/etc/hostname`, then the
`system.hostname` credential, then the name that is already set. An image with
`/etc/hostname` thus replaces this option. Build a shared image without a
static hostname. On NixOS, set `networking.hostName = ""`.

## Store root mode

`root_mode = "store"` gives each machine its own user namespace on a shared
image, also on CephFS. The driver does this:

1. It mounts a tmpfs at `<task dir>/nspawn-root` and creates a small skeleton
   (`etc`, `var/lib`, `usr/bin`, `bin`, `root`, `tmp`, `nix/var/nix`).
2. It runs `systemd-nspawn -D <tmpfs> --bind-ro=<image>/nix/store:/nix/store:idmap
   --kill-signal=SIGRTMIN+3 <toplevel>/init`. There is no `--boot` and no
   `--volatile`.
3. It unmounts the tmpfs in DestroyTask, or when the start fails.

Why: with `--volatile=overlay`, nspawn mounts the overlay first and then tries
to idmap it. Linux overlayfs has no `FS_ALLOW_IDMAP`, so the start fails with
`Failed to change mount attributes: Invalid argument`. Store mode uses no
overlay. The store is idmapped, which CephFS, ext4 and tmpfs support.

Rules in store mode:

* `private_users_ownership` is `map`. `auto` is refused, because it falls back
  to a recursive chown when idmap fails.
* `ephemeral`, `volatile`, `read_only`, `process_two`, `command` and
  `image_download` are refused. `boot` is ignored.
* User namespacing stays on with a group network namespace (bridge, Connect).
  Image mode still turns it off there, as upstream does. The machine cannot
  change links in a namespace that Nomad owns. Raw sockets (ping) fail.
* Machine writes go to the tmpfs and count against the machine memory limit.

The image should be a NixOS container system (`boot.isContainer = true`).

Always applied, without options:

* CPU weight: `CPUWeight` comes from the Nomad CPU shares. `AllowedCPUs` comes
  from the cpuset string when Nomad sends one.
* Memory: `MemoryMax` (or `MemoryHigh` plus `MemoryMax` with `memory_max`).
* Exec: `nomad alloc exec` and script checks set PATH, HOME, USER and LOGNAME
  inside the machine. A value in the task `environment` map wins.
* Stats: `TaskStats` reads the machine scope cgroup (`memory.current`,
  `memory.peak`, `memory.stat`, `cpu.stat`) on cgroup v2. It falls back to the
  executor process stats if the scope is not found.

## Example sandbox job

```hcl
job "sandbox" {
  type = "service"

  group "box" {
    count = 1

    network {
      mode = "bridge"
      port "ssh" {
        to = 22
      }
    }

    service {
      name     = "sandbox-ssh"
      port     = "ssh"
      provider = "consul"
    }

    task "machine" {
      driver = "nspawn"

      template {
        destination = "secrets/authorized_keys"
        data        = "ssh-ed25519 AAAA... agent@example"
      }

      config {
        image                   = "/var/lib/machines/sandbox"
        root_mode               = "store"
        unprivileged_port_start = 0
        resolv_conf             = "copy-uplink"
        port_map                = { ssh = 22 }
        cpu_hard_limit   = true
        tasks_max        = 4096
        load_credentials = {
          "ssh.authorized_keys.root" = "secrets/authorized_keys"
        }
      }

      resources {
        cpu    = 2000
        memory = 2048
      }
    }
  }
}
```

## Tests

Pure unit tests (no root, no systemd) are named `TestFleet...`:

```
go test ./nspawn/ -run Fleet
```
