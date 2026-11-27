import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { resolve } from "path";

// Vite builds directly into the Go embed directory so `make ui` produces the
// exact tree that go:embed bundles into the server binary. A console built with
// overlay modules in src/extensions/<id>/ sets GPUWORKS_UI_OUT_DIR to build
// somewhere else, so its bundle never lands in the community server's embed
// directory (`make ui` refuses an overlay without it). A relative path is
// taken from the repository root, as scripts/check_ui_bundle_size.sh reads it.
//
// The embed directory keeps a tracked .gitkeep so a checkout with no UI build
// still compiles (go:embed all:dist needs one file). emptyOutDir removes it
// with everything else, so public/.gitkeep puts it back on every build: the
// build then leaves the tracked file as it found it, and a commit made after
// `make ui` cannot delete it.
const outDir = resolve(__dirname, "..", process.env.GPUWORKS_UI_OUT_DIR || "internal/server/webui/dist");

export default defineConfig({
  resolve: { alias: { "@": resolve(__dirname, "src") } },
  plugins: [react(), tailwindcss()],
  build: {
    outDir,
    emptyOutDir: true,
  },
  server: {
    // Dev proxy: `npm run dev` talks to a locally running server on :8080.
    proxy: {
      // ws must stay on: the web terminal upgrades /api/v1/nodes/{uuid}/terminal
      // to a WebSocket, and with proxying disabled the handshake fails under
      // `npm run dev`.
      "/api": { target: "http://127.0.0.1:8080", ws: true, changeOrigin: true },
    },
  },
});
