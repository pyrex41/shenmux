// Gateway handshake logic, kept free of browser globals at import time so it
// can be exercised headlessly. Everything here is a pure function of the page
// identity the gateway stamped in and what the gateway said when a socket
// closed; the renderer in pixi-client.js supplies `document` and `location`.

// A browser cannot see the HTTP status of a failed websocket handshake, so a
// refusal is diagnosed by asking /api/instance afterwards. These are the two
// answers that mean this page will never be accepted again.
export const STALE_PAGE_MESSAGE =
  "stale page · this tab belongs to a previous shenmux web session · reload to continue";
export const UNAUTHORIZED_MESSAGE =
  "access refused · this page has no valid gateway token · open the URL printed by shenmux web";

// Identity stamped into the page by the gateway that served it. A tab left
// open from a previous `shenmux web` process carries the previous instance id.
export function pageIdentity(doc) {
  const meta = (name) => doc.querySelector(`meta[name="${name}"]`)?.content?.trim() || "";
  return { instance: meta("shenmux-instance"), token: meta("shenmux-token") };
}

// Where to ask the gateway who it is.
export function instanceEndpoint(token) {
  return token ? `/api/instance?token=${encodeURIComponent(token)}` : "/api/instance";
}

// Whether a closed socket is worth retrying. `identity` is the /api/instance
// body, or null when the gateway did not answer at all — which is a gateway
// that is down or restarting, i.e. a retry rather than a refusal. Only a
// gateway that answered and disowned this page is terminal.
export function refusalVerdict(identity, pageInstance) {
  if (identity && identity.instance && identity.instance !== pageInstance) {
    return { terminal: true, message: STALE_PAGE_MESSAGE };
  }
  if (identity && identity.authorized === false) {
    return { terminal: true, message: UNAUTHORIZED_MESSAGE };
  }
  return { terminal: false, message: "" };
}

export function socketURL(location, instance, token) {
  const scheme = location.protocol === "https:" ? "wss" : "ws";
  const query = new URLSearchParams({ instance });
  if (token) query.set("token", token);
  return `${scheme}://${location.host}/ws?${query}`;
}
