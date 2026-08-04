import * as esbuild from "esbuild";
import { copyFile } from "node:fs/promises";

await esbuild.build({
  entryPoints: ["../internal/webui/pixi-client.js"],
  bundle: true,
  minify: true,
  format: "iife",
  nodePaths: ["./node_modules"],
  outfile: "../internal/webui/app.bundle.js",
});

// The local workspace command reducer is also a WASI Component Model guest.
// Bundle its JCO-transpiled module separately so the browser can opt into the
// component without coupling it to the Pixi terminal bundle.
await esbuild.build({
  entryPoints: ["../internal/webui/workspace-component-entry.js"],
  bundle: true,
  minify: true,
  format: "esm",
  platform: "browser",
  nodePaths: ["./node_modules"],
  alias: { "shenmux:workspace/filesystem": "../internal/webui/workspace-filesystem.js" },
  external: ["node:fs/promises"],
  loader: { ".wasm": "file" },
  assetNames: "workspace-component-[hash]",
  outfile: "../internal/webui/workspace-component.js",
});

await copyFile(
  "../runtime/workspace-component/generated/workspace_component.core.wasm",
  "../internal/webui/workspace_component.core.wasm",
);
