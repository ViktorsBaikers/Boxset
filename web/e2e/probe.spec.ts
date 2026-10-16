import AxeBuilder from '@axe-core/playwright';
import { expect, test } from '@playwright/test';

declare const process: {
	env: Record<string, string | undefined>;
	versions: Record<string, string | undefined>;
};

const captureDir = process.env.BOXSET_CAPTURE_DIR;
const challenge = process.env.BOXSET_CAPTURE_CHALLENGE ?? '';
const origins = { dev: process.env.S5_DEV_URL, preview: process.env.S5_PREVIEW_URL };
const viewports = {
	desktop: { width: 1440, height: 900 },
	mobile: { width: 390, height: 844 }
};

test('runs in a Bun worker', () => {
	expect(process.versions.bun).toBeTruthy();
});

for (const mode of ['dev', 'preview'] as const) {
	for (const view of ['desktop', 'mobile'] as const) {
		const name = `${mode}-${view}`;
		const size = viewports[view];

		test.describe(name, () => {
			test.use({ viewport: size, deviceScaleFactor: 1 });

			test('activates by pointer then keyboard, passes axe and captures', async ({ page }) => {
				expect(captureDir, 'BOXSET_CAPTURE_DIR').toBeTruthy();
				expect(challenge, 'BOXSET_CAPTURE_CHALLENGE').toMatch(/^[0-9a-f]{16}$/);
				const origin = origins[mode];
				expect(origin, `origin of ${mode}`).toBeTruthy();

				const problems: string[] = [];
				page.on('console', (message) => {
					if (message.type() === 'error') problems.push(message.text());
				});
				page.on('pageerror', (error) => problems.push(error.message));

				// The first dev request can trigger one dependency-optimizer reload; settle it.
				await page.goto(origin!);
				await page.waitForLoadState('networkidle');
				await page.reload();
				await page.waitForLoadState('networkidle');

				const button = page.getByRole('button', { name: 'Add one activation' });
				const count = page.getByRole('status');
				await expect(button).toBeVisible();
				await expect(count).toHaveText('0');

				await button.click();
				await expect(count).toHaveText('1');
				await page.keyboard.press('Enter');
				await expect(count).toHaveText('2');
				await expect(button).toBeFocused();
				expect(await button.evaluate((element) => element.matches(':focus-visible'))).toBe(true);
				const ring = await button.evaluate((element) => getComputedStyle(element).outlineStyle);
				expect(ring).not.toBe('none');

				const axe = await new AxeBuilder({ page }).analyze();
				expect(axe.violations).toEqual([]);

				// The actual browser values, read before anything is injected.
				const actual = await page.evaluate(() => ({
					dpr: window.devicePixelRatio,
					width: window.innerWidth,
					height: window.innerHeight
				}));
				if (actual.dpr !== 1 || actual.width !== size.width || actual.height !== size.height) {
					const line = `S5-CAPTURE-VIEWPORT-MISMATCH name=${name} dpr=${actual.dpr} inner=${actual.width}x${actual.height}`;
					console.log(line);
					throw new Error(line);
				}

				// Challenge strip (66 bits: challenge MSB first, mode, viewport) and label.
				const bits = [
					...[...challenge].flatMap((digit) => {
						const value = parseInt(digit, 16);
						return [3, 2, 1, 0].map((shift) => (value >> shift) & 1);
					}),
					mode === 'preview' ? 1 : 0,
					view === 'mobile' ? 1 : 0
				];
				const label = `boxset-capture ${challenge} ${name} ${actual.width}x${actual.height}@${actual.dpr}`;
				const before = await page.evaluate(() => document.activeElement?.outerHTML);
				await page.evaluate(
					({ bits, label }) => {
						const strip = document.createElement('div');
						strip.setAttribute('aria-hidden', 'true');
						strip.dataset.boxsetCapture = 'strip';
						strip.style.cssText =
							'position:fixed;left:0;bottom:0;width:264px;height:16px;margin:0;padding:0;display:grid;grid-template-columns:repeat(33,8px);grid-template-rows:repeat(2,8px);pointer-events:none;z-index:2147483647';
						for (const bit of bits) {
							const cell = document.createElement('div');
							cell.style.cssText = `background:${bit ? 'rgb(0,0,0)' : 'rgb(255,255,255)'}`;
							strip.append(cell);
						}
						const text = document.createElement('div');
						text.setAttribute('aria-hidden', 'true');
						text.dataset.boxsetCapture = 'label';
						text.style.cssText =
							'position:fixed;left:0;bottom:16px;margin:0;padding:0;white-space:nowrap;font:11px/16px monospace;color:rgb(0,0,0);background:rgb(255,255,255);pointer-events:none;z-index:2147483647';
						text.textContent = label;
						document.body.append(strip, text);
					},
					{ bits, label }
				);

				const box = await button.boundingBox();
				expect(box).not.toBeNull();
				for (const part of ['strip', 'label']) {
					const other = await page.locator(`[data-boxset-capture="${part}"]`).boundingBox();
					expect(other).not.toBeNull();
					const apart =
						box!.x + box!.width <= other!.x ||
						other!.x + other!.width <= box!.x ||
						box!.y + box!.height <= other!.y ||
						other!.y + other!.height <= box!.y;
					expect(apart, `${part} overlaps the button`).toBe(true);
				}
				expect(await page.evaluate(() => document.activeElement?.outerHTML)).toBe(before);
				await expect(button).toBeFocused();
				expect(problems).toEqual([]);

				await page.screenshot({
					path: `${captureDir}/${name}.png`,
					fullPage: false,
					animations: 'disabled',
					scale: 'css'
				});
			});
		});
	}
}
