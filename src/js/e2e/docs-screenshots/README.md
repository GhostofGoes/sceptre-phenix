# Docs screenshots

`capture.spec.js` retakes the web UI screenshots in `docs/content/images`. Each
test is named after the image it writes, and it overwrites that image. It runs
only on demand, never in CI.

The pages read the sample data in `fixtures.json` instead of the server's: the
script answers the API requests named there (`"GET /hosts"` is
`GET /api/v1/hosts`) and lets every other request through. Any `phenix ui`
built with `VITE_AUTH=disabled` works, with an empty store and no minimega.
`TestDocsScreenshotFixturesMatchOpenAPI` in `src/go/web` checks each sample
response against its route's schema in `openapi.yml`, so a field renamed in the
API fails that test instead of drawing an empty column.

## Running

Start `phenix ui` on a free port (not 3000; see the root `AGENTS.md`), then:

```bash
cd src/js/e2e
npm ci
npx playwright install --with-deps chromium
E2E_BASE_URL=http://127.0.0.1:3080 npm run docs-screenshots

# one image
E2E_BASE_URL=http://127.0.0.1:3080 npm run docs-screenshots -- -g disks_table
```

`make docs-screenshots` from the repository root does the same, with
`E2E_BASE_URL` set. `DOCS_IMAGES_DIR` writes the images somewhere else, for
comparing them before replacing the committed ones.

`login_create` and `create_new_account` show the sign-in page, so they need a
`phenix ui` built with `VITE_AUTH=enabled` and started with
`--jwt-signing-key`. Run them on their own, with `E2E_BASE_URL` pointing at
that server:

```bash
E2E_BASE_URL=http://127.0.0.1:3081 npm run docs-screenshots -- -g 'login_create|create_new_account'
```

`settings_page` shows the server's own default settings, so it needs no
sample data.

Pages render at 1600 by 900 pixels at twice the pixel density. Review the new
images, and the alt text of each one in the docs, before committing them.

## Images of a running experiment

`live.spec.js` retakes the SCORCH runs page and State of Health graph images,
which need a real experiment. It runs only when `DOCS_LIVE_EXPERIMENT` names
the experiment, so `npm run docs-screenshots` skips it. The temporary
`Docs screenshots (live)` workflow (`.github/workflows/docs-screenshots-live.yml`)
runs it on a GitHub runner: it builds the `ntp` guest image, starts minimega
and `phenix ui`, creates and starts the `scorch-demo` experiment from
`live/topology.yaml` and `live/scenario.yaml` with `live/run-experiment.sh`,
and uploads the images as the `docs-screenshots-live` artifact. Copy them into
`docs/content/images` after looking at them.

## Adding an image

Add a test named after the image file, without `.png`. Put the responses the
page needs in `fixtures.json`, or pass a variant to `open()`, as `empty_table`
does. Images of a running experiment, the console, WebShark and the State of
Health graph need live minimega data or websockets, so they are still taken by
hand.
