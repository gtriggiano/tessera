<script setup lang="ts">
// The footer of the documentation pages. The default theme shows its footer
// only on pages without a sidebar, such as the home page: this component
// repeats the same text, from themeConfig.footer, at the bottom of the content
// of every other page, with the same look.
import { useData } from 'vitepress'

const { theme, frontmatter } = useData()
</script>

<template>
  <footer v-if="theme.footer && frontmatter.footer !== false" class="SiteFooter">
    <p v-if="theme.footer.message" class="message" v-html="theme.footer.message"></p>
    <p v-if="theme.footer.copyright" class="copyright" v-html="theme.footer.copyright"></p>
  </footer>
</template>

<style scoped>
.SiteFooter {
  margin-top: 48px;
  border-top: 1px solid var(--vp-c-gutter);
  padding-top: 32px;
  text-align: center;
}

.SiteFooter :deep(a) {
  text-decoration-line: underline;
  text-underline-offset: 2px;
  transition: color 0.25s;
}

.SiteFooter :deep(a:hover) {
  color: var(--vp-c-text-1);
}

.message,
.copyright {
  line-height: 24px;
  font-size: 14px;
  font-weight: 500;
  color: var(--vp-c-text-2);
}
</style>
