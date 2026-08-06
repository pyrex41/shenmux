import * as esbuild from "esbuild";
import { copyFile } from "node:fs/promises";

// The local workspace command reducer is a WASI Component Model guest. Bundle
// it separately so normal web builds can use the checked-in artifact without
// requiring a Rust toolchain or ignored JCO output in a clean checkout.
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
