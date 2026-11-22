import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { defineConfig, type HeadConfig } from 'vitepress'
import { llmsHead, writeLlmsFiles } from './llms'
import { mermaidFence } from './mermaid-fence'
import { nav, sidebar } from './navigation'
import { setReleaseVersion } from './release-version'
import { schemaPathFence } from './schema-path'

const repository = 'https://github.com/gtriggiano/tessera'

// A custom domain can build with DOCS_BASE=/; project Pages uses /<repo>/.
const base = process.env.DOCS_BASE ?? '/tessera/'
// The root of the whole site, where versions.json lives. A build whose base is
// below the root is an archived version, for example /tessera/v0.1/ or
// /tessera/next/ (see docs/scripts/assemble-site.mjs).
const siteRoot = process.env.DOCS_SITE_ROOT ?? base
// The tessera release this build documents, set by the workflows from the tag;
// empty for local and pull request builds, which document the last release.
const version = process.env.DOCS_VERSION ?? ''

for (const [name, value] of [['DOCS_BASE', base], ['DOCS_SITE_ROOT', siteRoot]]) {
  if (!value.startsWith('/') || !value.endsWith('/')) throw new Error(`${name} must start and end with /`)
}
if (!base.startsWith(siteRoot)) throw new Error('DOCS_BASE must be DOCS_SITE_ROOT or a folder below it')
if (version && !/^\d+\.\d+\.\d+(-rc\.\d+)?$/.test(version)) {
  throw new Error(`DOCS_VERSION must be X.Y.Z or X.Y.Z-rc.N (got ${version})`)
}
// The commit of a release documents that release, and the manifest names it. A
// release candidate is built from main, whose manifest names the last release:
// its pages show the candidate instead.
const manifest = JSON.parse(readFileSync(new URL('../../.release-please-manifest.json', import.meta.url), 'utf8'))
if (version && !version.includes('-') && version !== manifest['.']) {
  throw new Error(
    `DOCS_VERSION is ${version}, but this commit documents ${manifest['.']} (.release-please-manifest.json): build the documentation of ${version} from its tag`,
  )
}
const releaseVersion: string = version || manifest['.']
// Search engines index only the site root, and old pages are not edited.
const archived = base !== siteRoot

export default defineConfig({
  title: "tessera",
  description: "An external authorization service for Envoy.",
  lang: "en-US",
  base,
  lastUpdated: true,
  cleanUrls: false,
  ignoreDeadLinks: false,
  // head entries are not rewritten with the base, so prefix it here.
  head: [
    ["link", { rel: "icon", href: `${base}favicon.ico`, sizes: "32x32" }],
    [
      "link",
      { rel: "icon", type: "image/svg+xml", href: `${base}favicon.svg` },
    ],
    ["link", { rel: "apple-touch-icon", href: `${base}apple-touch-icon.png` }],
    ["meta", { property: "og:type", content: "website" }],
    [
      "meta",
      {
        property: "og:title",
        content: "tessera — External authorization for Envoy",
      },
    ],
    [
      "meta",
      {
        property: "og:description",
        content: "An external authorization service for Envoy.",
      },
    ],
    ["meta", { property: "og:image", content: `${base}og-image.png` }],
    ["meta", { name: "twitter:card", content: "summary_large_image" }],
    ...(archived
      ? [["meta", { name: "robots", content: "noindex" }] as HeadConfig]
      : []),
  ],
  // For AI agents (llms.ts): llms.txt and a Markdown twin of every page.
  transformHead: llmsHead,
  buildEnd: (siteConfig) => writeLlmsFiles(siteConfig, releaseVersion),
  markdown: {
    config: (md) => {
      mermaidFence(md);
      schemaPathFence(md, fileURLToPath(new URL("..", import.meta.url)), base);
      // The source reaches the parser with its includes already expanded.
      const parse = md.parse.bind(md);
      md.parse = (source, env) =>
        parse(setReleaseVersion(source, releaseVersion), env);
    },
  },
  vite: {
    // Read by the version selector and banner (theme/versions.ts).
    define: {
      __DOCS_SITE_ROOT__: JSON.stringify(siteRoot),
      __DOCS_VERSION__: JSON.stringify(version),
    },
    // Mermaid's diagram engines (elk, cytoscape, katex) are split into chunks
    // over 500 kB. They are loaded on demand, only by pages with diagrams.
    build: { chunkSizeWarningLimit: 2048 },
    // Pre-bundle mermaid for the dev server, so the first diagram does not
    // trigger a dependency re-optimization and a full page reload.
    optimizeDeps: { include: ["mermaid"] },
  },
  themeConfig: {
    logo: { src: "/logo.svg", alt: "tessera" },
    search: { provider: "local" },
    outline: [2, 3],
    nav,
    sidebar,
    socialLinks: [{ icon: "github", link: repository }],
    editLink: archived
      ? undefined
      : {
          pattern: `${repository}/edit/main/docs/:path`,
          text: "Edit this page on GitHub",
        },
    footer: {
      message: "Released under the Apache License 2.0.<br>Made with ❤️ in Florence, Italy.",
      copyright: "Copyright © 2026 Giacomo Triggiano",
    },
  },
});
