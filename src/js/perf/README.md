# phenix UI performance checks

Two industry-standard tools guard the web UI's load performance. CI runs both
in the `perf` job of `.github/workflows/frontend.yml`.

| Tool                                                           | Checks                                                                                                                                         | Config             |
| -------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------- | ------------------ |
| [size-limit](https://github.com/ai/size-limit)                 | Gzipped size of the entry JS and CSS, all JS chunks, and image assets in `../dist`                                                             | `.size-limit.cjs`  |
| [Lighthouse CI](https://github.com/GoogleChrome/lighthouse-ci) | Performance score, FCP, LCP, TBT, CLS, page weight, script size, compression and caching of static assets, for the main pages of a real server | `lighthouserc.cjs` |

## Running locally

```bash
cd src/js
npm ci
VITE_AUTH=disabled npm run build

cd perf
npm ci
npm run size          # bundle budgets; needs only the build above

# Lighthouse needs a running `phenix ui` serving that build (see ../e2e/README.md)
# and Chrome or Chromium (set CHROME_PATH if it is not found automatically)
E2E_BASE_URL=http://127.0.0.1:3000 npm run lighthouse
```

Lighthouse writes HTML and JSON reports to `lighthouse-results/`. CI uploads
them as the `lighthouse-reports` artifact.

## Changing a budget

The limits sit about 10% above the sizes and timings measured when they were
set. When a change trips one, first check whether it can be avoided: import
only what is used, lazy-load code needed by a single page, and register heavy
Buefy components locally in the view that uses them (see `src/utils/buefy.js`). Raise a
limit only deliberately, and say why in the pull request.

To see what is in a chunk, build with source maps and open the treemap:

```bash
cd src/js
VITE_AUTH=disabled npx vite build --sourcemap
npx source-map-explorer 'dist/assets/index-*.js'
```
