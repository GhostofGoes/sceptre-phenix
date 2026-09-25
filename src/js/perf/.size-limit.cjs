// Bundle size budgets for the built UI (run `npm run build` in src/js first).
// Sizes are gzipped unless noted, matching what the phenix server sends.
// Limits leave roughly 10% headroom over the sizes when they were set.
// Raise a limit only deliberately, and say why in the pull request.
module.exports = [
  {
    name: 'Entry JS (loaded on every page)',
    path: '../dist/assets/index-*.js',
    gzip: true,
    limit: '155 kB',
  },
  {
    name: 'Entry CSS',
    path: '../dist/assets/index-*.css',
    gzip: true,
    limit: '52 kB',
  },
  {
    name: 'All JS (entry and lazy-loaded views)',
    path: '../dist/assets/*.js',
    gzip: true,
    limit: '710 kB',
  },
  {
    name: 'Images (uncompressed)',
    path: '../dist/assets/*.{png,svg,jpg,jpeg,gif,webp}',
    brotli: false,
    limit: '120 kB',
  },
];
