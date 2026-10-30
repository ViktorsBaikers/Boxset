import { paraglideVitePlugin } from '@inlang/paraglide-js';
import adapter from '@sveltejs/adapter-static';
import { sveltekit } from '@sveltejs/kit/vite';
import tailwindcss from '@tailwindcss/vite';
import { playwright } from '@vitest/browser-playwright';
import { defineConfig } from 'vitest/config';

declare const process: { env: Record<string, string | undefined> };

// The production build is written under a placeholder base that the server rewrites to its
// runtime base at startup. Dev, preview and tests keep the root base.
const base = process.env.NODE_ENV === 'production' ? '/__boxset_base__' : '';

// The inlang project directory is written when it is loaded (its .lix store). The dev service
// mounts the source read-only, so deploy/dev/watch.ts points it at a writable copy.
const i18n = process.env.BOXSET_I18N_DIR ?? '.';

// A measured build pins its app version so the embedded shell is byte-identical
// across runs; an ordinary build keeps SvelteKit's per-build timestamp. The pin is a
// short opaque token: anything else is refused rather than written into the shell.
const buildVersion = process.env.BOXSET_BUILD_VERSION;
if (buildVersion && !/^[A-Za-z0-9._-]{1,64}$/.test(buildVersion)) {
	throw new Error(
		`BOXSET_BUILD_VERSION must be 1-64 characters of [A-Za-z0-9._-], got ${JSON.stringify(buildVersion)}`
	);
}
const version = buildVersion ? { name: buildVersion } : undefined;

export default defineConfig({
	// Keep component IDs logical so relative generated imports reach the .svelte-kit volume.
	// Native config imports use Bun's default canonicalization before this option is loaded.
	resolve: { preserveSymlinks: true },
	plugins: [
		tailwindcss(),
		sveltekit({
			adapter: adapter({
				pages: '../internal/server/dist',
				assets: '../internal/server/dist',
				fallback: '200.html',
				precompress: false
			}),
			paths: { base },
			version,
			compilerOptions: { runes: true }
		}),
		paraglideVitePlugin({ project: `${i18n}/project.inlang`, outdir: './.svelte-kit/paraglide' })
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
