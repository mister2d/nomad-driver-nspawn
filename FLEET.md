# Fleet additions

This fork adds options for jailed, ephemeral sandbox machines. Each option is
off or neutral by default. Existing jobs keep their behavior.

## Feature attribute

The fingerprint sets `driver.nspawn.features` to a sorted, comma separated
list: `cpu-hard-limit, cpu-weight, exec-env, load-credentials,
portmap-bridge, scope-stats, tasks-max`. A job can require a feature:

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
        image            = "/var/lib/machines/sandbox"
        boot             = true
        volatile         = "overlay"
        resolv_conf      = "copy-uplink"
        port_map         = { ssh = 22 }
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
