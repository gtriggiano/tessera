import { existsSync, readFileSync, statSync } from 'node:fs'
import { dirname, isAbsolute, relative, resolve, sep } from 'node:path'
import type { MarkdownRenderer } from 'vitepress'

// The configuration examples in docs/examples/ start with a directive of the
// YAML language server that names the JSON Schema by a path relative to the
// file:
//
//   # yaml-language-server: $schema=../public/config.schema.json
//
// so that an editor checks them against the schema of the working tree. A
// reader needs an address instead: when a page includes such a file with <<<,
// the build replaces the path with the address of the same file in this build
// of the site, which documents one version. llms.ts does the same in the
// Markdown twins. A relative path that does not lead to a file in
// docs/public/ fails the build.

// The origin of the published site, as in docs/scripts/check-links.mjs.
const origin = 'https://gtriggiano.github.io'
const directive = /^(#[ \t]*yaml-language-server:[ \t]*\$schema=)(\S+)[ \t]*$/gm

// publishSchemaPaths rewrites the relative schema paths of content, the text
// of file, for a build of srcDir with this base.
export function publishSchemaPaths(content: string, file: string, srcDir: string, base: string): string {
  const publicDir = resolve(srcDir, 'public')
  return content.replace(directive, (line: string, prefix: string, target: string) => {
    if (/^[a-z][a-z0-9+.-]*:/i.test(target) || isAbsolute(target)) return line
    const resolved = resolve(dirname(file), target)
    const path = relative(publicDir, resolved)
    if (path.startsWith('..') || isAbsolute(path) || !existsSync(resolved)) {
      throw new Error(`${file}: the schema path ${target} must lead to a file in docs/public/`)
    }
    return prefix + origin + base + path.split(sep).join('/')
  })
}

export function schemaPathFence(md: MarkdownRenderer, srcDir: string, base: string): void {
  const fallback = md.renderer.rules.fence
  if (!fallback) throw new Error('schema-path: markdown-it fence renderer not found')

  md.renderer.rules.fence = (tokens, idx, options, env, self) => {
    // VitePress gives the token of a <<< include the file in src, and reads the
    // file in its own fence renderer, which this one wraps: with the content
    // set here and src removed, it renders the content as it is.
    const token = tokens[idx] as (typeof tokens)[number] & { src?: [string, string] }
    const [file, region] = token.src ?? []
    if (file && !region && existsSync(file) && statSync(file).isFile()) {
      const content = readFileSync(file, 'utf8').replace(/\r\n/g, '\n')
      const published = publishSchemaPaths(content, file, srcDir, base)
      if (published !== content) {
        // The development server watches the included files.
        env?.includes?.push(file)
        token.content = published
        token.src = undefined
      }
    }
    return fallback(tokens, idx, options, env, self)
  }
}
