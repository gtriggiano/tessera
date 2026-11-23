import type { DefaultTheme } from 'vitepress'

// Top navigation and per-section sidebars for the sections: Guide (/guide/)
// and Reference (/reference/), and later Architecture (/architecture/).
// List only pages that exist: each documentation change appends the pages it
// adds (a nav entry when a section gets its first page, sidebar items as pages
// land), so the site never links to a missing page.

export const nav: DefaultTheme.NavItem[] = [
  { text: 'Guide', link: '/guide/what-is-tessera', activeMatch: '^/guide/' },
  { text: 'Reference', link: '/reference/configuration', activeMatch: '^/reference/' },
]

export const sidebar: DefaultTheme.SidebarMulti = {
  '/guide/': [
    {
      text: 'Get started',
      items: [
        { text: 'What is tessera?', link: '/guide/what-is-tessera' },
        { text: 'How tessera works', link: '/guide/how-tessera-works' },
      ],
    },
    {
      text: 'Set up',
      items: [
        { text: 'Client address', link: '/guide/client-address' },
        { text: 'Enrichments', link: '/guide/enrichments' },
        { text: 'Sets', link: '/guide/sets' },
        { text: 'Policy rules', link: '/guide/policy-rules' },
        { text: 'Responses and logs', link: '/guide/responses-and-logs' },
      ],
    },
  ],
  '/reference/': [
    {
      text: 'Reference',
      items: [
        { text: 'Configuration', link: '/reference/configuration' },
        { text: 'Expression language', link: '/reference/expressions' },
        { text: 'CLI', link: '/reference/cli' },
        { text: 'gRPC API', link: '/reference/grpc-api' },
        { text: 'Admin endpoints', link: '/reference/admin-endpoints' },
        { text: 'Metrics', link: '/reference/metrics' },
        { text: 'Logs', link: '/reference/logs' },
        { text: 'Failure modes', link: '/reference/failure-modes' },
      ],
    },
    {
      text: 'Releases',
      items: [
        { text: 'Versioning', link: '/reference/versioning' },
        { text: 'Changelog', link: '/reference/changelog' },
      ],
    },
  ],
}
