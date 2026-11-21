import DefaultTheme from 'vitepress/theme'
import type { Theme } from 'vitepress'
import { h } from 'vue'
import Mermaid from './Mermaid.vue'
import SiteFooter from './SiteFooter.vue'
import VersionBanner from './VersionBanner.vue'
import VersionSelector from './VersionSelector.vue'
import './custom.css'

export default {
  extends: DefaultTheme,
  // The version selector and banner (versions.ts) go in the default layout's
  // slots. The home page has no doc-before slot, so its banner goes in the hero.
  // The default footer is hidden on pages with a sidebar: SiteFooter shows the
  // same text at the bottom of those pages.
  Layout: () =>
    h(DefaultTheme.Layout, null, {
      'nav-bar-content-after': () => h(VersionSelector),
      'nav-screen-content-after': () => h(VersionSelector, { screen: true }),
      'doc-before': () => h(VersionBanner),
      'home-hero-info-before': () => h(VersionBanner),
      'doc-after': () => h(SiteFooter),
    }),
  enhanceApp({ app }) {
    app.component('Mermaid', Mermaid)
  },
} satisfies Theme
