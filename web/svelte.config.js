import adapter from '@sveltejs/adapter-static';

export default {
	kit: {
		// The panel is a single page served by the Go binary, which falls back
		// to index.html for every path that is not part of the API.
		adapter: adapter({
			pages: '../internal/webui/build/ui',
			assets: '../internal/webui/build/ui',
			fallback: 'index.html'
		}),
		output: { bundleStrategy: 'single' }
	}
};
