# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Changed

- **Web UI**: Users has a search box and can sort by First Name. Error popups are wider and show each step of a nested server error on its own line, the root cause last in bold.
- **Web UI**: On the SCORCH pipelines page the experiment name sits in a box colored by the experiment's state (green running, red stopped, yellow starting or stopping), clicking a node opens its output window at once with a loading spinner, and the node name in that window is outlined.
- **API**: The SCORCH pipelines response includes the experiment's state (`experiment.status`). SCORCH component output and terminal endpoints now require permission to get the experiment.
- **Hosts**: Disk usage for each host is measured at most once a minute, so the Hosts page no longer queues two minimega mesh commands per host every 10 seconds. Disk usage shows as a percentage.
- **Web UI**: On the experiment page the VM rows appear first and screenshots fill in as the server takes them, with a loading image meanwhile. Uptime is narrower, and Uptime and Delay can be sorted. The Netflow tab is always shown (saying when there are no captures yet), the netflow button shows a spinner while a capture starts and sits left of the State of Health button.
- **API**: Experiment listings report start progress (`percent`) for starting experiments. VM lists sent over the WebSocket no longer carry screenshots; they follow as `experiment/vm/screenshot` updates. VM lists can sort by `delayed`.
- **Web UI**: Disks explain in tooltips which experiments use a disk and why an action is unavailable. The footer documentation button uses a book icon.
- **Web UI**: The Disks table has Size on Disk and a sortable Virtual Size column, an Actions column (snapshot, clone, download, rename, delete), and a narrower Name, In Use and search box. Sort arrows now sit beside the heading in every table.
- **Web UI**: A help button in the footer links to the phēnix documentation, and the Tunneler page links to its documentation page.
- **API**: Disk listings name the experiments that use each disk (`experiments`, with whether each is running), including disks that back the images an experiment uses. This replaces the `experiment` field, which was never set.
- **Web UI**: The SCORCH pipelines page can clear the status of a run that is not running, with a Clear button per run and Clear all beside Stop all (`POST /experiments/{name}/scorch/pipelines/{run}/clear`). Go to experiment now sits next to Back to SCORCH.
- **Web UI**: The SCORCH component output window shows the component name on its own line, with the experiment, run name, stage and live status beneath it, so long titles no longer get cut off.
- **Web UI**: Disks has an upload dialog like Configs, with drag and drop and the accepted file types listed; unsupported files are rejected with an error.
- **Web UI**: Hosts can sort by RAM Used, RAM Total and Uptime, the "# of VMs" column is now "VMs", and sort arrows sit beside headings instead of over them.
- **Web UI**: The experiment page is cached, so it opens at once from any page, and shows "Loading experiment…" when nothing is cached yet.
- **Web UI**: The Scorch page has a button beside each SCORCH start/stop button that opens the experiment's pipelines. The Configs kind filter has an "All kinds" option.
- **Web UI**: The Configs table can sort by Name and Last Updated, sort arrows sit next to the column titles, and Last Updated also shows how long ago the config changed, for example "(3 hours ago)". The kind filter and search box are visibly separate.
- **Web UI**: Leaving the config editor, or a new config, without changing anything no longer asks about discarding edits.
- **CLI / Web UI**: Display the release version or source branch alongside the commit hash and build timestamp in the version output and footer.

### Fixed

- **Web UI**: Stopping an experiment from the SCORCH page (or the Experiments page) no longer flips back to "started" when a list requested before the stop arrives after it.
- **Web UI**: Users: Role sorting works; editing a user and closing the dialog no longer changes the row; a rejected edit keeps the dialog open; a failed delete no longer leaves a spinner; roles stay shown after updates from other sessions; resource names are no longer split into characters after a user is created; password errors are highlighted.
- **Web UI**: Server error text in popups is escaped rather than rendered as HTML.
- **SCORCH**: Fixed a data race between component output updates and new output streams, and on terminal ownership checks.
- **Web UI**: Refreshing the Experiments page while a large experiment starts no longer hangs on "Loading experiments": the list no longer waits on minimega while an experiment starts or stops, and shows the starting experiment's progress.
- **Web UI**: Sorting VMs on the experiment page no longer waits for every VM to be screenshotted.
- **Web UI**: A disk's details now show the experiments using it, marking stopped ones, instead of always "N/A". Size sorting on Disks is correct for sizes in bytes and minimega's size format.
- **Web UI**: Sorting Hosts by number of VMs now works.
- **Web UI**: Configs open for viewing and editing in well under a second instead of several. The viewer reuses the config data it already fetched (and fetches on hover), the editor reuses the viewer's copy, and the Ace editor loads in the background in one round of requests. Download buttons show a spinner at once.
- **Web UI**: Viewing a Topology config with builder data no longer errors, an invalid upload now reports its error, and the config search treats characters such as `(` literally.
- **Web UI**: The Builder tab opens several times faster on a remote server (about 3.4 s to 0.7 s over a 100 ms link). mxGraph's loader fetched each of its ~140 source files separately, so the page made 182 requests; the server now serves the builder's scripts as one bundle it builds from the same files, leaving 26 requests.
- **Web UI**: Fixed the Scorch page: the table no longer breaks when an experiment starts or stops, or when the search contains characters such as `(`. Exiting a terminal now clears its button, and terminals that closed while the page was away no longer linger. Request failures are reported.
- **Web UI**: The SCORCH runs page no longer errors on run updates that arrive before it loads. It picks up runs added after loading, reports a failed terminal exit, and stops a component's previous output stream before starting another.
- **Web UI**: Saving a config reports permission and network failures in the usual error notification instead of failing silently; validation errors still open the editor's error dialog.
- **Web UI**: The paginate toggles on Disks, Experiments, Hosts and Users remember the choice again.
- **Web UI**: The Settings page has a Reset Form button that discards unsaved changes.
- **Web UI**: A failed SCORCH run now shows its error in a notification, and failures to start or stop a SCORCH run are reported instead of ignored. The server now sends the run's actual error, and humanized app errors no longer reach the UI as an empty object.
- **Web UI**: Error notifications show their icon again.
- **Web UI**: The browser console no longer fills with debug output (API responses, uploaded file objects, pipeline layout steps, websocket chatter). Leftover debug logging was removed, development-only diagnostics now appear only in development builds, real failures log as warnings or errors, and lint rejects new `console.log` calls.
- **Web UI**: Empty tables say why they are empty (still loading, failed to load, nothing matches the search, or nothing exists yet) instead of "Your search turned up empty!".
- **Web UI**: Paginate toggles sit above their tables instead of below them.
- **Web UI**: The Console page no longer says console access is not configured while the console is starting, and names the real reason when it cannot start.
- **Web UI**: Pages no longer stall for seconds the first time they are opened. The UI prefetches each page's code once idle, and the server now gzip-compresses static assets, marks Vite's hashed assets as immutable, and sends content-hash ETags so other static files (noVNC, xterm.js, the topology builder) are revalidated instead of re-downloaded.
- **Web UI**: Leaving the Logs page before its logs finish loading no longer throws an error.
- **Web UI**: Reduced the JavaScript loaded on every page by about a quarter (602 kB to 441 kB, 180 kB to 142 kB gzipped) by registering only the Buefy components the UI uses, and cut bundled image weight from 1.6 MB to about 110 kB.
- **Web UI**: Running experiments no longer re-render the whole VM table for every VM screenshot update, and ignore screenshots from other experiments.
- **Web UI**: The Logs page filters streamed log lines incrementally and debounces search, instead of re-filtering every loaded log on each message or keystroke.
- **Web UI**: The Scorch page loads experiments' apps in parallel and only once.
- **Web UI**: Fixed leaks when leaving pages: SCORCH terminals, SCORCH output sockets, State of Health graph simulations, and the VM file browser's unload listener are now cleaned up.
- **Web UI**: Hosts and VM tiles pages no longer poll while the browser tab is hidden.
- **Web UI**: Cached permission checks are reset when a different role logs in.
- **Web UI**: Fixed a stopped experiment's schedule update throwing when a VM was not found.
- **Web UI**: API responses (JSON, YAML, and plain text) are gzip-compressed for browsers that accept it, so large lists such as VMs, disks, and logs download faster on slow links.
- **Web UI / Server**: Leaving a running experiment now stops the server from screenshotting its VMs every 5 seconds. Those screenshots kept minimega busy for the rest of the session, which made other pages, especially Hosts and Disks, wait on a loading spinner.
- **Web UI**: Pages no longer hide behind a full-page spinner while their data loads. A status in the header shows when the page's data was last loaded, or that it is loading, and has a refresh button that reloads it without reloading the whole page. The Experiments, Configs, Disks, Hosts, Users, Logs, Scorch, and VM tiles pages show the data they last loaded straight away when you return to them. Pages cancel their requests when you leave. Hosts no longer spins forever when there are no hosts, Disks no longer spins forever after a failed load, and a stopped experiment's host and disk choices no longer repeat or reload with every search.
- **Web UI**: After login, the UI loads the Experiments, Configs, Users, Logs, Hosts, Scorch, Settings, and Disks tabs in the background, two requests at a time, so they open with data already in place. A page's load keeps running when you leave it, for up to two minutes, so the data is ready when you return.
- **CI**: The Frontend workflow cancels its superseded runs when new commits are pushed, like the other workflows. Closing or merging a pull request cancels the CI still running for it.
- **Disks**: When `qemu-img` is installed (it is in the phenix container), phenix inspects disk images itself, in parallel, instead of running one minimega `disk info` per image in turn. It also skips images whose files have not changed since they were last inspected, so listing disks no longer holds up other minimega commands. Images are inspected at startup and whenever the images directory changes. Open Disks pages update on their own when images change, and the Disks refresh button has the server inspect every image again. An image is marked in use when a process holds a lock on it, as minimega does, but a backing image no longer reports the in-use state of the image built on it.
- **Web UI**: Tables start unpaginated in a fresh browser. Each table's Paginate toggle is remembered separately in the browser's local storage, shared by every user of that browser and kept through logout and reloads; it is no longer stored per user.
- **Web UI**: The Settings page shows its last-loaded settings straight away, with the header's refresh button to reload them. Reset Form restores the loaded settings at once instead of fetching them again, and Reset Form and Save Changes are disabled until something changes.
- **Web UI**: The Scorch table keeps its column headings on one line when there is room, gives the Experiment column normal width, centers the status and terminal buttons under their headings, and puts the Experiment sort arrow next to its heading.
- **Web UI**: The SCORCH pipeline page has a Back to SCORCH button, the experiment's name at the top, and Go to experiment, Start all and Stop all buttons. Start all starts the runs that are not running one after another, since SCORCH runs one at a time; Stop all stops the running run and cancels the rest. Each run is titled with its name and has a start/stop button next to its status that spins until the run starts or stops. The previous-loop button only shows when there is an earlier loop to return to. The page shows its last-loaded runs straight away when you return to it.
- **Web UI**: The Scorch table has separate start/stop buttons next to the experiment and SCORCH statuses, spinning while a request is in flight, and a Go to experiment button. The SCORCH button's tooltip names the run it starts or stops. The terminal button only shows when the experiment has a SCORCH terminal. Fixed the experiment status failing to render for users who cannot start experiments.
- **Web UI**: Removed unused pipeline drawing code and a dead component.

### Added

- **SCORCH**: `POST /experiments/{name}/scorch/pipelines/{run}/cleanup` runs only a run's top-level cleanup stage, and the pipelines response marks runs with `hasCleanup`. The SCORCH pipeline page shows a Cleanup button for runs that define cleanup components.
- **Web UI**: Bundle size budgets (size-limit) and Lighthouse CI audits run in CI; see `src/js/perf/README.md`.

## [1.0.0]

### Changed

- Removed the unsupported `Printer` and `Server` topology node types.

### Fixed

- **Tunneler**:
  - **Listener State Synchronization**: Centralized local listener tracking behind a synchronized manager to prevent concurrent map and state access.
  - **Operation Reporting**: Return errors for unknown listeners and local port conflicts instead of reporting successful operations.
  - **Argument Handling**: Reject malformed command arguments and Unix socket payloads without panicking.
  - **Listener Moves**: Restore an active listener's original port when moving to a new port fails.
  - **Listener IDs**: Maintain a direct ID index and prevent IDs from being reused during a tunneler session.
  - **Listener Listing**: Return local listeners in deterministic ID order.
  - **Duplicate Listeners**: Treat repeated listener creation events as idempotent without replacing active listeners.
  - **Unix Socket Protocol**: Replace Gob messages with JSON and typed listener action payloads.

### Added

- **Tunneler Web Interface**: Add a local web dashboard for listing, enabling, disabling, and moving listeners, with live WebSocket updates.
- **CLI Command Aliases**: Added `exp del`, `exp trig`, `exp res`, `exp rec`, `image del`, `config del`, and `vm res`.
- **Centralized Logging Architecture**: Implemented a unified logging system where phēnix core aggregates logs from internal services and external apps.
- **Dynamic Configuration**: Integrated `viper` with `fsnotify` to allow hot-swapping of configuration settings (e.g., log levels) without restarting services.
- **Log Rotation**: Configurable log rotation settings (`max-size`, `max-backups`, `max-age`) for the persistent system log.
- **Example Applications**:
  - **Documentation**: Consolidated all example documentation into a single `examples/README.md`. Added sections on the "App Contract", developer usage, and common pitfalls.
  - **CI Integration**: Added a new GitHub Actions workflow (`.github/workflows/examples.yml`) and Makefile targets (`make examples`) to automatically build and test the examples.
  - **Python Example**:
    - Includes panic simulation logic to demonstrate structured error logging.
    - Added comprehensive unit tests (`test_app.py`) covering configuration, modification, and crash recovery.
  - **Go Example**:
    - Uses core `phenix/types` and `phenix/store` packages for robust configuration parsing.
    - Includes panic recovery middleware to log stack traces as structured JSON.
    - Supports dynamic log levels via `PHENIX_LOG_LEVEL`.
    - Added unit tests (`main_test.go`) using the subprocess pattern to verify CLI behavior, panic recovery, and log output.
- **Documentation**:
  - Comprehensive `README.md` updates including architecture diagrams, configuration tables, and developer guides.
  - Added dependency installation instructions (Go, Python, Node, Protoc) for local development.
  - Added developer guidelines for Python app error handling (raise vs sys.exit).
- **Build Tools**: Added `make docker` target for easier container builds.
- **Build System**: Standardized Makefiles with consistent targets (`help`, `all`, `test`, `lint`, `format`, `clean`) and improved help output.
- **Code Quality**: Integrated `golangci-lint` with a comprehensive ruleset (`.golangci.yml`) and fixed numerous static analysis issues. (Note: Some linters are currently disabled to facilitate incremental adoption).
- **Shell Completion**: Added `phenix completion` command for Bash, Zsh, Fish, and PowerShell.
- **Docker Wrapper**: Added `make install-wrapper` to support shell completion when running via Docker.
- **Web UI (Vue 3)**: Upgraded the web frontend to Vue 3 (Vuex → Pinia, vue-resource → axios, vue-cli → Vite, Buefy 1.0). Page components now load dynamically for a quicker initial load, and the codebase was reorganized (page components moved to `views/`).
- **Idle Timeout / Auto-Logout**: Added a configurable inactivity timeout that warns and then logs the user out. Managed from the web UI **Settings** page and backed by a new `GET /api/v1/settings/timeout` route.
- **Podman CI**: Added a GitHub Actions job that builds the Podman `Containerfile` (through the Go build stage) so the Podman build path can't break unnoticed.
- **Experiment Lifecycle Triggers**: Added `phenix exp trigger` for manually invoking app lifecycle stages. Deprecated `phenix exp trigger-running`; use `phenix exp trigger running` instead. The command errors out if a given app isn't part of the experiment or if the requested lifecycle stage isn't applicable to it, and shell completion only suggests apps applicable to the given experiment and stage.

### Changed

- **Log Output**: Default log output format changed to structured JSON on `stderr` for applications.
- **Configuration Management**: Moved from static flags/env vars to a watched `config.yaml` file managed via `phenix settings` commands.
- **CLI Commands**: Separated runtime configuration (`phenix settings`) from persistent database management (`phenix settings db`). Replaced `reset` command with `unset --all`.
- **CLI UX**: Added helpful error message when `phenix settings unset` is called without arguments.
- **Configuration Precedence**: Enforced `Flag > File > Env > Default` precedence for runtime settings. This ensures `phenix settings set` commands correctly override Docker environment variables.
- **Hot-Swapping**: Enabled runtime configuration updates for `log.console` and `ui.logs.level` without requiring a service restart.
- **Dependencies**: Updated Go modules to version 1.24 to leverage loop variable safety fixes and `slog` support.
- **Refactor**: Removed legacy/redundant `ui.log-level`, `ui.log-verbose`, and `ui.logs.phenix-path` configuration settings.
- **Refactor**: Renamed `log.output` to `log.console` and `log.file.*` to `log.system.*` to clarify their purpose (Human vs Machine).
- **Refactor**: Removed deprecated `ui.unix-socket-endpoint` and `ui.minimega-path` flags.
- **Refactor**: Removed noisy debug logs from HTTP handlers to improve log clarity. Use the `--log-requests` flag with the `ui` command to see HTTP traffic logs instead.
- **Refactor**: Updated `vrouter` app to use structured logging instead of `fmt.Printf`.
- **Refactor**: Replaced `go-bindata` with Go 1.16+ `embed` package for asset embedding, removing the build dependency on `go-bindata`.
- **Performance**: Removed excessive debug logging from hot paths in log file cache management to reduce I/O overhead during high-frequency UI polling.
- **Web UI Build**: Replaced yarn with npm and `vue-cli` with Vite; the UI build toolchain now targets Node 24.
- **Web UI Environment Variables**: Build-time UI environment variables are now prefixed `VITE_` instead of `VUE_APP_` (e.g. `VITE_AUTH` replaces `VUE_APP_AUTH`, and `VITE_BASE_PATH` replaces `VUE_BASE_PATH`). The Docker `PHENIX_WEB_AUTH` / `PHENIX_BASE_PATH` build args are unchanged.

### Removed

- **Legacy Tests**: Removed outdated `testing/` directory and unused `*_test.go` files (replaced by `examples/`).

### Fixed

- **Web UI VM Interfaces**: Preserve the experiment's configured bridge when reconnecting a disconnected VM interface.
- **Web UI**: Removed non-functional packet-capture controls from the IP column header and renamed the IPv4 column to IP.
- **Web UI VNC Tab**: Excluded external/HIL nodes and "Do Not Boot" (DNB) nodes from the VNC tab in the running experiment view.
- **VM Snapshots**: Replaced deprecated minimega `vm migrate` commands with `vm save` and `vm config state` for snapshot, restore, and redeploy workflows.
- **Image Script Updates**: Prevented `phenix image update` from duplicating refreshed scripts in `script_order`.
- **Timestamp Consistency**: Enforced `2006-01-02 15:04:05.000` time format across file logs.
