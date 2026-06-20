import { defineConfig, loadEnv } from "vite";
import react from "@vitejs/plugin-react";
import path from "path";
// /api/* is proxied to the oss-agent HTTP API (set VITE_API_TARGET to override).
export default defineConfig(function (_a) {
    var mode = _a.mode;
    var env = loadEnv(mode, process.cwd(), "");
    var target = env.VITE_API_TARGET || "http://localhost:7634";
    return {
        plugins: [react()],
        resolve: { alias: { "@": path.resolve(__dirname, "src") } },
        server: {
            port: 5317,
            proxy: {
                "/api": { target: target, changeOrigin: true, rewrite: function (p) { return p.replace(/^\/api/, ""); } },
            },
        },
    };
});
