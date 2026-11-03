// Helpers the docs screenshot specs share: where the images go, and how an
// element, a region or a ringed control is captured.
const path = require('node:path');

const IMAGES =
  process.env.DOCS_IMAGES_DIR ??
  path.resolve(__dirname, '../../../../docs/content/images');

const image = (name) => path.join(IMAGES, `${name}.png`);

async function shoot(locator, name) {
  await locator.screenshot({
    path: image(name),
    animations: 'disabled',
    caret: 'hide',
  });
}

// Captures the smallest rectangle holding every locator, with pad pixels
// around it. The region must fit in the viewport: the boxes are measured
// against it, so the first locator is scrolled into view first.
async function shootRegion(page, locators, name, pad = 8) {
  await locators[0].scrollIntoViewIfNeeded();
  const boxes = await Promise.all(locators.map((l) => l.boundingBox()));
  const left = Math.max(0, Math.min(...boxes.map((b) => b.x)) - pad);
  const top = Math.max(0, Math.min(...boxes.map((b) => b.y)) - pad);
  const right = Math.max(...boxes.map((b) => b.x + b.width)) + pad;
  const bottom = Math.max(...boxes.map((b) => b.y + b.height)) + pad;
  await page.screenshot({
    path: image(name),
    clip: { x: left, y: top, width: right - left, height: bottom - top },
    animations: 'disabled',
    caret: 'hide',
  });
}

// Draws a red ring around a control, as the docs mark the one to click.
async function ring(locator) {
  await locator.evaluate((el) => {
    const r = el.getBoundingClientRect();
    const mark = document.createElement('div');
    Object.assign(mark.style, {
      position: 'fixed',
      left: `${r.left - 6}px`,
      top: `${r.top - 6}px`,
      width: `${r.width + 12}px`,
      height: `${r.height + 12}px`,
      border: '3px solid #e8412c',
      borderRadius: '50%',
      pointerEvents: 'none',
      zIndex: '10000',
    });
    document.body.append(mark);
  });
}

module.exports = { IMAGES, image, shoot, shootRegion, ring };
