# docs

The Recon design record, built with [Astro](https://astro.build) and
[Starlight](https://starlight.astro.build).

```sh
pnpm install
pnpm dev      # http://localhost:4321
pnpm build    # ./dist
```

Content lives in `src/content/docs/architecture/`. The sidebar is generated from that directory, and
the order comes from each page's `sidebar.order` frontmatter.

Two conventions worth keeping, both from the [console copy rules](src/content/docs/architecture/console.md):
no em dashes in prose, and sentence case in headings.
