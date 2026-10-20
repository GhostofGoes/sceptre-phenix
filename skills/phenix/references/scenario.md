# Scenario Reference

Detail behind the Scenario summary in the [skill body](../SKILL.md): app
run scheduling, scenario inheritance, and the app catalog.

## `runPeriodically`

`runPeriodically: <duration>` on an app entry re-runs that app's `running`
stage on an interval. It is only honored when the experiment is started from
the web UI/API, or from the CLI with
`phenix experiment start --honor-run-periodically`; a bare
`phenix experiment start` parses the value and then ignores it.

With that flag the CLI stays in the foreground re-running each app's
`running` stage on its interval until the process is interrupted
(SIGINT/SIGTERM) or the experiment is stopped from any process, so it is not
usable as a fire-and-forget start. The periodic runs of the web UI also end
when the experiment is stopped from another process, within five seconds or
once the app's current `running` stage ends, and a dry run's run as a dry run.
Default apps are never run periodically, and an unparsable duration, or one
shorter than one second, is logged and skipped rather than failing the start.

## `fromScenario`

`fromScenario` lets one scenario inherit an app's `assetDir`, `metadata`,
`hosts`, `disabled`, and `runPeriodically` from another stored scenario config
by name:

```yaml
spec:
  apps:
    - name: my-app
      fromScenario: base-scenario   # copy my-app's config from scenario/base-scenario
```

The referenced scenario must have a `topology` annotation matching the
topology this scenario is used with, and must contain an app with the same
`name`. This is how a shared "base" scenario can be layered under
per-experiment overrides without duplicating app config.

## Apps

Built-in apps that always run for every experiment, in this order and before
the scenario's apps in every stage but `running`: `ntp`, `serial`, `startup`,
`vrouter`. A node opts out of them with the
`phenix/default-apps: false` annotation (see
[annotations.md](annotations.md)).

Optional apps are added explicitly in the scenario's `apps` list. Examples:
`user-shell`, SCORCH, monitoring apps like `packetbeat`/`elasticsearch`,
`caldera`, `scale`, `wireguard`, `otsim`, `helics`. Many live in the companion
`sceptre-phenix-apps` repo (Python apps under `phenix_apps/apps/`). List the
apps a running phenix can see with `phenix experiment apps`.

For per-app metadata schemas and the SCORCH scenario format, pull in
[Sceptre Phenix Scenario Configuration](https://phenix.sceptre.dev/latest/configuration/#scenario)
and [Apps](https://phenix.sceptre.dev/latest/apps/) as needed.
