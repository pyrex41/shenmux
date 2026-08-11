import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { MAX_COLS, MAX_ROWS, MIN_COLS, MIN_ROWS, MIN_SCALE, fitGrid, renderScale } from "./fit.mjs";

// The metrics the renderer's bitmap font produces at the shipped font size.
const CELL = { cellWidth: 10, lineHeight: 19 };
// Sub-pixel slack: an exact fit is computed through a division, and being a
// billionth of a pixel over is not the overflow these tests are about.
const fitsWithin = (used, available) => used <= available + 1e-6;

describe("fitGrid", () => {
  it("never asks for a grid wider than the box it was given", () => {
    // The bug this replaces: columns were counted against a width that still
    // included the padding, so the canvas drew past the window edge.
    for (const width of [374, 736, 1248, 1888]) {
      const { cols, scale } = fitGrid({ width, height: 760, ...CELL });
      assert.ok(fitsWithin(cols * CELL.cellWidth * scale, width), `${width}px fitted ${cols} columns`);
    }
  });

  it("never asks for a grid taller than the box it was given", () => {
    for (const height of [200, 760, 1002]) {
      const { rows, scale } = fitGrid({ width: 1248, height, ...CELL });
      assert.ok(fitsWithin(rows * CELL.lineHeight * scale, height), `${height}px fitted ${rows} rows`);
    }
  });

  it("fills a desktop window at natural size", () => {
    const fit = fitGrid({ width: 1888, height: 1002, ...CELL });
    assert.equal(fit.scale, 1);
    assert.equal(fit.cols, 188);
    assert.equal(fit.rows, 52);
  });

  it("shrinks the glyphs rather than hand a phone an unusable column count", () => {
    const fit = fitGrid({ width: 374, height: 766, ...CELL });
    assert.ok(fit.scale < 1);
    assert.ok(fit.cols >= MIN_COLS, `expected at least ${MIN_COLS} columns, got ${fit.cols}`);
  });

  it("shrinks for height too, so a landscape phone keeps usable rows", () => {
    const fit = fitGrid({ width: 1248, height: 150, ...CELL });
    assert.ok(fit.scale < 1);
    assert.ok(fit.rows >= MIN_ROWS, `expected at least ${MIN_ROWS} rows, got ${fit.rows}`);
  });

  it("stops shrinking at MIN_SCALE and fits fewer columns instead", () => {
    // Legibility wins over the column minimum: a window this narrow gets a
    // grid that still fits, just a small one.
    const fit = fitGrid({ width: 120, height: 300, ...CELL });
    assert.equal(fit.scale, MIN_SCALE);
    assert.ok(fitsWithin(fit.cols * CELL.cellWidth * fit.scale, 120));
  });

  it("clamps to the sanity bounds on an enormous window", () => {
    const fit = fitGrid({ width: 100000, height: 100000, ...CELL });
    assert.equal(fit.cols, MAX_COLS);
    assert.equal(fit.rows, MAX_ROWS);
  });

  it("never returns a degenerate grid for a collapsed box", () => {
    const fit = fitGrid({ width: 0, height: 0, ...CELL });
    assert.equal(fit.cols, 1);
    assert.equal(fit.rows, 1);
  });
});

describe("renderScale", () => {
  it("draws a grid that already fits at its natural size", () => {
    assert.equal(renderScale({ width: 1248, height: 760, cols: 80, rows: 24, ...CELL }), 1);
  });

  it("does not magnify a grid smaller than the window", () => {
    assert.equal(renderScale({ width: 1888, height: 1002, cols: 40, rows: 10, ...CELL }), 1);
  });

  it("shrinks an observer's view of a grid it may not resize", () => {
    // A read-only client on a phone cannot take the PTY down to 40 columns,
    // so the only honest alternative to clipping is smaller glyphs.
    const scale = renderScale({ width: 374, height: 766, cols: 60, rows: 24, ...CELL });
    assert.ok(scale < 1);
    assert.ok(fitsWithin(60 * CELL.cellWidth * scale, 374));
  });

  it("bottoms out at MIN_SCALE rather than becoming illegible", () => {
    const scale = renderScale({ width: 374, height: 766, cols: 300, rows: 24, ...CELL });
    assert.equal(scale, MIN_SCALE);
  });
});
