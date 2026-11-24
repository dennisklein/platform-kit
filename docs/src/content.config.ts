import { defineCollection } from 'astro:content';
import { z } from 'astro/zod';
import { docsLoader } from '@astrojs/starlight/loaders';
import { docsSchema } from '@astrojs/starlight/schema';
import { categoryIds } from './data/categories.mjs';

export const collections = {
  docs: defineCollection({
    loader: docsLoader(),
    schema: docsSchema({
      extend: z.object({
        /** Page type. It selects the page template and the checks in scripts/check-pages.mjs. */
        type: z
          .enum(['landing', 'start', 'kind', 'controller', 'cli', 'lab', 'concept', 'workflow', 'howto', 'runbook', 'tutorial', 'reference', 'project'])
          .optional(),
        /**
         * The category link id of this page: `[](kind:Host)` resolves to the page with `{cat: kind, id: Host}`.
         * `aliases` are old ids of a renamed page: links that still use them resolve to this page.
         */
        xref: z.object({ cat: z.enum(categoryIds), id: z.string(), aliases: z.array(z.string()).optional() }).optional(),
        /**
         * Old paths of a renamed page, without the base: `[operate/day-1/old-name/]`.
         * astro.config.mjs makes a redirect from each to this page.
         */
        redirectFrom: z.array(z.string()).optional(),
        /**
         * More ids that this page owns, each at an anchor of the page: `act: [PowerCycle]`
         * makes `[](act:PowerCycle)` go to `#powercycle`. A map gives the anchor and the
         * title: `ctrl: { rolloutproposer: { anchor: the-proposer, title: Rollout proposer } }`.
         */
        anchors: z
          .record(
            z.string(),
            z.union([
              z.array(z.string()),
              z.record(z.string(), z.union([z.string(), z.object({ anchor: z.string(), title: z.string().optional() })])),
            ]),
          )
          .optional(),
        /** Text above the title. The default comes from the page type. */
        eyebrow: z.string().optional(),
        /** Day 1 (install) or day 2 (operation) for how-to pages. */
        day: z.union([z.literal(1), z.literal(2)]).optional(),
        /** Source files that the page describes, as `/repo/<path>`. */
        sources: z.array(z.string()).default([]),
      }),
    }),
  }),
};
