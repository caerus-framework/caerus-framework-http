# Configuration reload and the restart policy

`cf_http` listens on settings that **cannot rebind in place**. When the `http`
configuration source reloads, the server applies what it can live and leaves
the rest for a new process — it never re-binds the listener mid-flight.

## What is live vs restart-required

- **Live (rebinds immediately):** `metrics_enabled` — the `MetricsProvider`
  toggles on the next scrape.
- **Restart-required:** `address`, `read_timeout_sec`, `write_timeout_sec`,
  `idle_timeout_sec`, `read_header_timeout_sec`, `max_header_bytes`,
  `shutdown_timeout_sec`. The active listener stays on the old values until a
  new process binds them.
- `knob_restart_policy` itself applies on the next reload; it only selects how
  a settings change is handled.

The server sets `http_server_restart_required=1` when a restart-required knob
changed and logs at ERROR level. In both policies the change is **not** applied
to the running listener.

## knob_restart_policy

```json
{
  "address": ":8080",
  "knob_restart_policy": "handled"
}
```

### `handled` (default)

The process stays alive on the old bind. Operators roll the Deployment / poke
the process to pick up the new config. **`handled` means "file changed ≠ port
changed until you restart."**

### `immediate`

The server stops `Run` gracefully (drain + shutdown) so the process exits and
systemd/k8s starts a fresh instance that binds the new settings. The library
never calls `os.Exit` — `Run` returns `ErrServerKnobsRestart` and the framework
finishes the shutdown.

```json
{
  "address": ":8080",
  "knob_restart_policy": "immediate"
}
```

Unknown policy values are rejected at config validation.

## Anti-pattern

Do not assume Caerus config reload is Viper-style "everything live." Reload
reconnects and re-reads config; listening sockets are owned by the process and
only a restart changes them. This is the **rotation plane** for credentials
(External Secrets mounts) and the **restart plane** for server knobs.

## Copy-paste: ops rollout

1. Update `config/http.json` in the mounted Secret/ConfigMap.
2. `handled`: watch `/metrics` for `http_server_restart_required=1`, then
   rollout the Deployment.
3. `immediate`: the pod exits itself; confirm the Deployment restarts and the
   new listener binds the new settings.
