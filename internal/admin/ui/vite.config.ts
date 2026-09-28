import { svelte } from '@sveltejs/vite-plugin-svelte'
import { defineConfig, type Plugin } from 'vite'
import { fileURLToPath } from 'node:url'
import { mkdir, readdir, rm } from 'node:fs/promises'
import { join } from 'node:path'

const adminUiDir = fileURLToPath(new URL('.', import.meta.url))
const distDir = fileURLToPath(new URL('../dist', import.meta.url))
const assetsDir = join(distDir, 'assets')

// dist/index.html is a committed, hash-free shell. admin.go reads the
// Vite manifest and injects the hashed asset references at serve time,
// so no generated filename ever has to be committed. Only the generated
// subtrees are cleared here; the shell and its .gitkeep are preserved.
async function cleanGeneratedOutput(): Promise<void> {
  const entries = await readdir(assetsDir).catch(() => [] as string[])
  await Promise.all(
    entries.filter((name) => name !== '.gitkeep').map((name) => rm(join(assetsDir, name), { recursive: true, force: true }))
  )
  await rm(join(distDir, '.vite'), { recursive: true, force: true })
  await mkdir(assetsDir, { recursive: true })
}

function preserveShell(): Plugin {
  return {
    name: 'contextdb-preserve-admin-shell',
    buildStart: {
      order: 'pre',
      handler: cleanGeneratedOutput,
    },
  }
}

export default defineConfig({
  root: adminUiDir,
  base: '/admin/',
  plugins: [preserveShell(), svelte()],
  build: {
    outDir: '../dist',
    // The committed shell lives in outDir, so Vite must not wipe it.
    emptyOutDir: false,
    manifest: true,
    rollupOptions: {
      input: fileURLToPath(new URL('src/main.ts', import.meta.url)),
    },
  },
})
