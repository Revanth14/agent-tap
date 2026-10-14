import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// The daemon the dev server proxies /api to. Its WebSocket origin check only
// accepts its own origin, so the proxy presents that origin instead of the
// dev server's; the check itself is never relaxed.
const daemon = process.env.ATAP_URL ?? "http://127.0.0.1:9090";

export default defineConfig({
  base: "/ui/",
  plugins: [react(), tailwindcss()],
  server: {
    host: "127.0.0.1",
    proxy: {
      "/api": {
        target: daemon,
        changeOrigin: true,
        ws: true,
        configure(proxy) {
          proxy.on("proxyReqWs", (proxyReq) => proxyReq.setHeader("origin", daemon));
        },
      },
    },
  },
});
