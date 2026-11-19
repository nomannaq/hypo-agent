import { rmSync, writeFileSync } from "node:fs"
import { resolve } from "node:path"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig, type Plugin } from "vite"

const hotFile = resolve(import.meta.dirname, "../public/hot")
// Explicit IPv4 + non-default port: DDEV's router already holds 127.0.0.1:5173,
// and `localhost` can resolve to either stack.
const devURL = "http://127.0.0.1:5174"

// Tells the Go server (gonertia) to load assets from the Vite dev server,
// same trick as laravel-vite-plugin.
const hot = (): Plugin => ({
  name: "hot-file",
  configureServer(server) {
    server.httpServer?.once("listening", () =>
      writeFileSync(hotFile, devURL)
    )
    const clean = () => rmSync(hotFile, { force: true })
    process.on("exit", clean)
    process.on("SIGINT", () => process.exit())
    process.on("SIGTERM", () => process.exit())
  },
})

export default defineConfig({
  plugins: [react(), tailwindcss(), hot()],
  resolve: {
    alias: {
      "@": resolve(import.meta.dirname, "./src"),
    },
  },
  server: {
    host: "127.0.0.1",
    port: 5174,
    strictPort: true,
    origin: devURL,
  },
  build: {
    manifest: "manifest.json",
    outDir: "../public/build",
    emptyOutDir: true,
    rollupOptions: { input: ["src/app.css", "src/app.tsx"] },
  },
})
