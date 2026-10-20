# phenix Web API Reference

`phenix ui` serves the web UI and REST API together on the same port —
`0.0.0.0:3000` by default (`ui.listen-endpoint` / `--listen-endpoint`,
`PHENIX_UI_LISTEN_ENDPOINT`). All routes below are relative to the `/api/v1`
base path unless noted.

Prefer the equivalent `phenix` CLI command over calling the web API directly
unless the HTTP interface is specifically needed (e.g. scripting against a
running `phenix ui` server, or building a UI integration).

When phenix runs in its Docker container, the bundled compose file uses
`network_mode: host`, so `localhost:3000` on the host reaches it directly
with no port mapping.

## Authentication

Authentication depends on `phenix ui --jwt-signing-key` (`ui.jwt-signing-key`,
`PHENIX_UI_JWT_SIGNING_KEY`):

- **No signing key (the default, including the bundled Docker compose file):**
  auth is disabled. Every request is served as the built-in `global-admin` role
  and no token is required or checked. The server logs
  `no JWT signing key provided -- disabling auth` at startup.
- **Signing key set:** a JWT is required. Obtain one from `/login` (registered
  for both `GET` with HTTP basic auth and `POST` with a JSON body) and send it
  on every request in the `X-Phenix-Auth-Token` header — NOT the standard
  `Authorization` header, which is left free for proxy authentication. The token
  may also be passed as a `?token=<jwt>` query parameter, which is how browser
  links and image tags reach `/screenshot.png` and `/vnc/ws`.
- `--jwt-signing-key proxy-jwt` accepts JWTs minted by an upstream proxy, and a
  `dev|`-prefixed key enables development auth.

```bash
TOKEN=$(curl -s -u admin:password http://localhost:3000/api/v1/login | jq -r .token)
curl -H "X-Phenix-Auth-Token: $TOKEN" http://localhost:3000/api/v1/experiments
```

## Routes

| Resource | Routes |
|---|---|
| Configs | `GET/POST /configs`, `GET/PUT/DELETE /configs/{kind}/{name}`, `POST /configs/download` |
| Schemas | `GET /schemas/{version}`, `GET /schemas/{kind}/{version}` |
| Experiments | `GET/POST /experiments`, `GET /experiments/{name}`, `PATCH /experiments/{name}`, `DELETE /experiments/{name}`, `POST /experiments/{name}/start`, `POST /experiments/{name}/stop`, `GET /experiments/{name}/apps`, `POST/PUT /experiments/builder` |
| Experiment detail | `GET /experiments/{name}/topology`, `GET /experiments/{name}/topology/search`, `POST/DELETE /experiments/{name}/trigger`, `GET/POST /experiments/{name}/schedule`, `GET /experiments/{name}/soh` (state of health), `GET /experiments/{name}/captures`, `GET /experiments/{name}/files`, `GET /experiments/{name}/files/{filename}` |
| Netflow | `GET/POST/DELETE /experiments/{exp}/netflow`, `GET /experiments/{exp}/netflow/ws` |
| Subnet captures | `POST /experiments/{exp}/captureSubnet`, `POST /experiments/{exp}/stopCaptureSubnet` |
| VMs | `GET/PATCH /experiments/{exp}/vms`, `GET/PATCH/DELETE /experiments/{exp}/vms/{name}`, plus `/start`, `/stop`, `/restart`, `/redeploy`, `/shutdown`, `/reset`, `/cdrom`, `/vnc`, `/vnc/ws`, `/screenshot.png`, `/captures`, `/snapshots`, `/snapshots/{snapshot}`, `/commit`, `/memorySnapshot`, `/forwards`, `/forwards/{host}/{port}/ws` |
| VM mount (only with the `vm-mount` feature enabled) | `POST /experiments/{exp}/vms/{name}/mount`, `DELETE .../unmount`, `GET .../files?path=`, `GET .../files/download?path=`, `PUT .../files/upload?path=`, `POST .../files/copy` |
| Disks | `GET/POST/DELETE /disks`, `/disks/snapshot`, `/disks/rebase`, `/disks/resize`, `/disks/commit`, `/disks/clone`, `/disks/rename`, `/disks/download` |
| Misc lookups | `GET /vms` (all experiments), `GET /applications`, `GET /topologies`, `GET /topologies/{topo}/scenarios`, `GET /hosts` |
| Users/Roles/Auth | `GET/POST /users`, `GET/PATCH/DELETE /users/{username}`, `POST /users/{username}/tokens`, `GET /roles`, `POST /signup`, `GET/POST /login`, `GET /logout` |
| Realtime | `GET /ws` (websocket broker for UI events/logs), `GET /logs`, `POST /console`, `GET /console/{pid}/ws`, `POST /console/{pid}/size` |
| SCORCH | `GET /experiments/{name}/scorch/pipelines`, `GET /experiments/{name}/scorch/pipelines/{run}/{loop}`, `POST/DELETE /experiments/{name}/scorch/pipelines/{run}`, `GET /experiments/{name}/scorch/components/{run}/{loop}/{stage}/{cmp}[/ws]`, `/experiments/{name}/scorch/terminals*` |
| Settings | `GET/POST /settings`, `GET /settings/password`, `GET /settings/timeout` |
| Builder | `GET/POST /builder/topologies`, `GET/PUT /builder/topologies/{name}`; the builder UI itself is served from the server root as `GET /builder` and `POST /builder/save` (outside `/api/v1`). Payloads and workflow are in [`builder.md`](builder.md) |
| Workflow | `POST /workflow/apply/{branch}` (`?dryRun=true`, `pending`, `expect`, `tag`), `POST /workflow/configs/{branch}` (`?dryRun=true`). Parameters and answers are in [`workflow.md`](workflow.md#rest-endpoints) |
| Options | `GET /options` (server-side CLI defaults like bridge-mode/deploy-mode, plus `base-dir.injects` and `workflow-dry-run`) |

`POST /experiments/{name}/scorch/pipelines/{run}` answers 204 and runs the
SCORCH run in the background. It answers 409 when a SCORCH run of the
experiment already executes (one started from the CLI included) or the
experiment is being stopped, and 400 for a run ID the scenario does not have,
a run naming a component the app does not define, a stopped experiment or one
without a scorch app, as `phenix experiment scorch` refuses them.

The SCORCH component output and terminal routes need `experiments` `get` on
the experiment and answer 403 without it, but for the terminal list
(`GET /experiments/{name}/scorch/terminals`), which leaves out the terminals
of experiments the role cannot get. Opening a terminal's read/write websocket,
which types into it, and exiting the terminal also need `experiments/trigger`
`create` (403 otherwise); a role without it is handed a read-only terminal.

When a config fails its schema, the config routes (`POST /configs`,
`PUT /configs/{kind}/{name}`) and the workflow routes answer 400 with the
explained lines in `metadata.validation`, one per line, and the validator's
text in `metadata.validation-raw`; see [`workflow.md`](workflow.md).

`src/go/web/server.go` is the authoritative route list.
