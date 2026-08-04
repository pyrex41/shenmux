import * as esbuild from "esbuild";

await esbuild.build({
  entryPoints: ["../internal/webui/pixi-client.js"],
  bundle: true,
  minify: true,
  format: "iife",
  nodePaths: ["./node_modules"],
  outfile: "../internal/webui/app.bundle.js",
});
