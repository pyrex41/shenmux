package webui

import "embed"

// FS contains the zero-build browser client. Keeping the first client as
// embedded assets makes the prototype deployable as one shenmux-web binary.
//
//go:embed index.html app.bundle.js style.css workspace.html workspace.js workspace-runtime.js workspace.css workspace-component.js workspace_component.core.wasm
var FS embed.FS
