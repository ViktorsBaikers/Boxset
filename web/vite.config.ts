import { paraglideVitePlugin } from '@inlang/paraglide-js';
import adapter from '@sveltejs/adapter-static';
import { sveltekit } from '@sveltejs/kit/vite';
import { playwright } from '@vitest/browser-playwright';
import { defineConfig } from 'vitest/config';

export default defineConfig({
	plugins: [
		sveltekit({ adapter: adapter(), compilerOptions: { runes: true } }),
		paraglideVitePlugin({ project: './project.inlang', outdir: './.svelte-kit/paraglide' })
	],
	test: {
		expect: { requireAssertions: true },
		include: ['src/**/*.svelte.test.ts'],
		browser: {
			enabled: true,
			headless: true,
			provider: playwright(),
			instances: [{ browser: 'chromium' }]
		}
	}
});
