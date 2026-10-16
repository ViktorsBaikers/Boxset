import { defineConfig } from '@playwright/test';

export default defineConfig({
	testDir: 'e2e',
	testMatch: '*.spec.ts',
	fullyParallel: false,
	workers: 1,
	retries: 0,
	forbidOnly: true,
	reporter: [['list'], ['json', { outputFile: 'test-results/report.json' }]],
	use: { headless: true }
});
