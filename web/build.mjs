import * as esbuild from "esbuild";

// The committed bundle is the build output that ships; `--outfile` lets the
// drift guard build the same inputs somewhere disposable and diff the two
// without disturbing the working tree.
const args = process.argv.slice(2);
const flag = args.indexOf("--outfile");
const outfile = flag === -1 ? "../internal/webui/app.bundle.js" : args[flag + 1];
if (!outfile) throw new Error("--outfile requires a path");

await esbuild.build({
  entryPoints: ["../internal/webui/pixi-client.js"],
  bundle: true,
  minify: true,
  format: "iife",
  nodePaths: ["./node_modules"],
  outfile,
});
