// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import starlightLinksValidator from 'starlight-links-validator';
import { satteri } from '@astrojs/markdown-satteri';
import baseLinks from './scripts/base-links.mjs';
import xref from './scripts/xref.mjs';
import glossary from './scripts/glossary.mjs';
import { loadRedirects, loadRegistry } from './scripts/registry.mjs';
import { sidebar } from './scripts/sidebar.mjs';
import { site, base, repoUrl, branch } from './scripts/site.mjs';

// Links to planned pages (src/data/xref-planned.yaml, while it exists) resolve, but the
// page is not there yet. The validator skips them.
const planned = new Set([...loadRegistry(base).values()].filter((e) => e.planned).map((e) => e.href.split('#')[0]));

// Anchors that components make: the validator sees only the anchors of Markdown headings.
// scripts/check-xrefs.mjs checks these in dist/ instead.
//   #spec.x, #status.x   field tables (kind pages)
//   #--flag, #-f          flag tables (command pages); #exit-1 exit status tables
//   /phases/#…, /conditions/#…, /labels-annotations/#…, /glossary/#…   catalog rows
const componentAnchor = /#((spec|status)\.|--?[a-z]|[a-z]+\.--?[a-z]|exit-\d)|\/reference\/(phases|conditions|labels-annotations|glossary)\/#/;

// Fonts of the "Control room" theme: Inter for text, IBM Plex Mono for code and names.
const fonts = [
  '@fontsource-variable/inter/wght.css',
  '@fontsource/ibm-plex-mono/400.css',
  '@fontsource/ibm-plex-mono/500.css',
  '@fontsource/ibm-plex-mono/600.css',
];

const overrides = {
  Head: './src/components/overrides/Head.astro',
  PageTitle: './src/components/overrides/PageTitle.astro',
  Hero: './src/components/overrides/Hero.astro',
  Footer: './src/components/overrides/Footer.astro',
  Header: './src/components/overrides/HeaderTabs.astro',
  Sidebar: './src/components/overrides/SidebarTabs.astro',
};

export default defineConfig({
  site,
  base,
  // Old paths of renamed pages (`redirectFrom` frontmatter), so old links and bookmarks work.
  redirects: loadRedirects(base),
  outDir: process.env.PK_DOCS_OUT || './dist',
  markdown: {
    processor: satteri({
      hastPlugins: [xref({ base }), glossary({ base }), baseLinks({ base })],
      // Keep -- and ... as typed: flags like --yes and quoted output must stay exact.
      features: { smartPunctuation: false },
    }),
  },
  integrations: [
    starlight({
      title: 'pk-metal',
      description: 'pk-metal installs Talos Linux on bare-metal servers through their BMCs, with Kubernetes objects and a human approval step.',
      favicon: '/favicon.svg',
      social: [{ icon: 'github', label: 'GitHub', href: repoUrl }],
      editLink: { baseUrl: `${repoUrl}/edit/${branch}/docs/` },
      lastUpdated: true,
      customCss: [...fonts, './src/styles/tokens.css', './src/styles/components.css', './src/styles/theme.css'],
      expressiveCode: { themes: ['github-dark-default', 'github-light-default'] },
      components: overrides,
      sidebar: sidebar({ base }),
      plugins: [
        starlightLinksValidator({
          errorOnRelativeLinks: false,
          exclude: ({ link }) => planned.has(link.split('#')[0]) || componentAnchor.test(link),
        }),
      ],
    }),
  ],
});
