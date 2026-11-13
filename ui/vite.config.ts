import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { resolve } from "path";

// Vite builds directly into the Go embed directory so `make ui` produces the
// exact tree that go:embed bundles into the server binary.
export default defineConfig({
  resolve: { alias: { "@": resolve(__dirname, "src") } },
  plugins: [react(), tailwindcss()],
  build: {
    outDir: resolve(__dirname, "../internal/server/webui/dist"),
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
