import assert from "node:assert/strict";
import { describe, it } from "node:test";

import {
  STALE_PAGE_MESSAGE,
  UNAUTHORIZED_MESSAGE,
  instanceEndpoint,
  pageIdentity,
  refusalVerdict,
  socketURL,
} from "./gateway.mjs";

// Only the subset of the DOM that pageIdentity touches. The point of the
// extraction is that no real browser is needed to test this.
const documentWithMetas = (metas) => ({
  querySelector(selector) {
    const match = /^meta\[name="(.+)"\]$/.exec(selector);
    if (!match) throw new Error(`unexpected selector: ${selector}`);
    const name = match[1];
    return Object.hasOwn(metas, name) ? { content: metas[name] } : null;
  },
});

describe("pageIdentity", () => {
  it("reads the instance and token the gateway stamped in", () => {
    const doc = documentWithMetas({ "shenmux-instance": "inst-1", "shenmux-token": "tok-1" });
    assert.deepEqual(pageIdentity(doc), { instance: "inst-1", token: "tok-1" });
  });

  it("trims surrounding whitespace", () => {
    const doc = documentWithMetas({ "shenmux-instance": "  inst-1\n", "shenmux-token": " tok-1 " });
    assert.deepEqual(pageIdentity(doc), { instance: "inst-1", token: "tok-1" });
  });

  it("yields empty strings when the meta tags are absent", () => {
    assert.deepEqual(pageIdentity(documentWithMetas({})), { instance: "", token: "" });
  });

  it("yields an empty token when the gateway served the page without one", () => {
    const doc = documentWithMetas({ "shenmux-instance": "inst-1", "shenmux-token": "" });
    assert.deepEqual(pageIdentity(doc), { instance: "inst-1", token: "" });
  });
});

describe("refusalVerdict", () => {
  it("keeps retrying when the gateway did not answer at all", () => {
    // A gateway that is merely restarting must not be given up on.
    assert.deepEqual(refusalVerdict(null, "inst-1"), { terminal: false, message: "" });
  });

  it("keeps retrying when the gateway still owns this page", () => {
    const identity = { instance: "inst-1", authorized: true };
    assert.equal(refusalVerdict(identity, "inst-1").terminal, false);
  });

  it("keeps retrying when the gateway answered without an opinion", () => {
    assert.equal(refusalVerdict({}, "inst-1").terminal, false);
  });

  it("stops when the gateway reports a different instance", () => {
    // A tab left open across a `shenmux web` restart will never be accepted.
    const verdict = refusalVerdict({ instance: "inst-2", authorized: true }, "inst-1");
    assert.equal(verdict.terminal, true);
    assert.equal(verdict.message, STALE_PAGE_MESSAGE);
  });

  it("stops when the gateway refuses this page's token", () => {
    const verdict = refusalVerdict({ instance: "inst-1", authorized: false }, "inst-1");
    assert.equal(verdict.terminal, true);
    assert.equal(verdict.message, UNAUTHORIZED_MESSAGE);
  });

  it("reports staleness ahead of authorization when both apply", () => {
    // A stale tab is expected to fail its token check too; "reload" is the
    // actionable message, so it wins.
    const verdict = refusalVerdict({ instance: "inst-2", authorized: false }, "inst-1");
    assert.equal(verdict.message, STALE_PAGE_MESSAGE);
  });

  it("does not treat an authorized:true answer from an unnamed gateway as terminal", () => {
    assert.equal(refusalVerdict({ authorized: true }, "inst-1").terminal, false);
  });
});

describe("instanceEndpoint", () => {
  it("omits the query when there is no token", () => {
    assert.equal(instanceEndpoint(""), "/api/instance");
  });

  it("carries the token so the gateway can judge this page", () => {
    assert.equal(instanceEndpoint("tok-1"), "/api/instance?token=tok-1");
  });

  it("escapes token characters that would otherwise alter the query", () => {
    assert.equal(instanceEndpoint("a&b=c d"), "/api/instance?token=a%26b%3Dc%20d");
  });
});

describe("socketURL", () => {
  it("builds a ws url carrying instance and token", () => {
    const location = { protocol: "http:", host: "127.0.0.1:8789" };
    assert.equal(socketURL(location, "inst-1", "tok-1"), "ws://127.0.0.1:8789/ws?instance=inst-1&token=tok-1");
  });

  it("upgrades to wss on a secure page", () => {
    const location = { protocol: "https:", host: "example.test" };
    assert.equal(socketURL(location, "inst-1", "tok-1"), "wss://example.test/ws?instance=inst-1&token=tok-1");
  });

  it("omits the token parameter when the page has none", () => {
    const location = { protocol: "http:", host: "127.0.0.1:8789" };
    assert.equal(socketURL(location, "inst-1", ""), "ws://127.0.0.1:8789/ws?instance=inst-1");
  });

  it("escapes instance and token", () => {
    const location = { protocol: "http:", host: "127.0.0.1:8789" };
    assert.equal(socketURL(location, "a b", "x&y"), "ws://127.0.0.1:8789/ws?instance=a+b&token=x%26y");
  });
});
