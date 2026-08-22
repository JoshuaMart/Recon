<script lang="ts">
	/**
	 * The icon set, in one file.
	 *
	 * Every glyph is an inline SVG with a viewBox and no intrinsic size, so it
	 * fills whatever box it lands in unless something sizes it. app.css sets a
	 * base size on `svg` for that reason: a context rule nobody wrote costs a
	 * wrong size and never a full-card arrow.
	 */
	interface Props {
		name:
			| 'search'
			| 'feed'
			| 'programs'
			| 'queue'
			| 'settings'
			| 'clock'
			| 'trend'
			| 'lineage'
			| 'chevron'
			| 'download'
			| 'open';
	}

	const { name }: Props = $props();

	const paths: Record<Props['name'], string> = {
		search: 'M11 4a7 7 0 100 14 7 7 0 000-14M20 20l-4.3-4.3',
		feed: 'M4 12h4l2-5 3 10 2-5h5',
		programs: 'M7 4h10a3 3 0 013 3v10a3 3 0 01-3 3H7a3 3 0 01-3-3V7a3 3 0 013-3M8 9h8M8 13h5',
		// Stacked bars of falling length: work waiting, which is what a depth is.
		queue: 'M4 6h16M4 12h11M4 18h6',
		settings:
			'M12 9a3 3 0 100 6 3 3 0 000-6M12 3v3M12 18v3M3 12h3M18 12h3M5.6 5.6l2.1 2.1M16.3 16.3l2.1 2.1M18.4 5.6l-2.1 2.1M7.7 16.3l-2.1 2.1',
		clock: 'M12 4a8 8 0 100 16 8 8 0 000-16M12 8v4l3 2',
		trend: 'M4 17l5-6 4 3 7-8',
		lineage: 'M6 4v7a3 3 0 003 3h9M15 11l3 3-3 3',
		chevron: 'M9 6l6 6-6 6',
		download: 'M12 4v10M8 11l4 4 4-4M5 19h14',
		// The square with an arrow leaving it: what a reader already reads as "opens
		// elsewhere", so the link needs no label beside the URL it sits next to.
		open: 'M14 4h6v6M20 4l-8 8M18 14v4a2 2 0 01-2 2H6a2 2 0 01-2-2V8a2 2 0 012-2h4'
	};
</script>

<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true">
	<path d={paths[name]} />
</svg>
