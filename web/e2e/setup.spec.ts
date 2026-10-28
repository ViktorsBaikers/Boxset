import AxeBuilder from '@axe-core/playwright';
import { expect, test, type Page } from '@playwright/test';

const origins = { root: process.env.S5_DEV_URL, media: process.env.S5_PREVIEW_URL };
const token = 'synthetic-browser-setup-credential';
const password = 'synthetic-browser-password';

async function fill(page: Page, credential = token) {
	await page.getByLabel('Setup token').fill(credential);
	await page.getByLabel('Username').fill('owner');
	await page.getByLabel('Password', { exact: true }).fill(password);
}

async function inspect(page: Page) {
	const scan = await new AxeBuilder({ page })
		.withTags(['wcag2a', 'wcag2aa', 'wcag21aa', 'wcag22aa'])
		.analyze();
	expect(scan.violations).toEqual([]);
	expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
}

for (const [base, origin] of Object.entries(origins)) {
	test(`${base}: typed server validation maps only known fields to translated errors`, async ({
		page
	}) => {
		expect(origin).toBeTruthy();
		await page.route('**/api/v1/setup', (route) =>
			route.fulfill({
				status: 422,
				contentType: 'application/problem+json',
				body: JSON.stringify({
					status: 422,
					title: 'Unprocessable Content',
					code: 'validation_failed',
					errors: [
						{ location: 'body.username', code: 'invalid', message: 'server-message-secret' },
						{ location: 'body.property-secret', code: 'invalid', message: 'server-message-secret' }
					]
				})
			})
		);
		await page.goto(`${origin}setup`);
		await fill(page);
		await page.getByLabel('Username').fill('hidden' + String.fromCodePoint(0x202e) + 'format');
		await page.getByRole('button', { name: 'Create admin' }).click();
		const username = page.getByLabel('Username');
		await expect(username).toBeFocused();
		await expect(username).toHaveAttribute('aria-describedby', 'setup-username-error');
		await expect(
			page.getByText('Use 1 to 64 characters without control or formatting characters.')
		).toBeVisible();
		await expect(page.getByLabel('Password', { exact: true })).toHaveValue('');
		await expect(page.locator('body')).not.toContainText('server-message-secret');
		await expect(page.locator('body')).not.toContainText('property-secret');
	});
	test(`${base}: setup states reflow under production CSP`, async ({ browser }) => {
		test.setTimeout(180000);
		expect(origin).toBeTruthy();
		for (const width of [320, 768, 1024, 1440]) {
			for (const colorScheme of ['dark', 'light'] as const) {
				for (const reducedMotion of ['reduce', 'no-preference'] as const) {
					for (const input of ['keyboard', 'pointer']) {
						const context = await browser.newContext({
							viewport: { width, height: 1000 },
							colorScheme,
							reducedMotion
						});
						try {
							const page = await context.newPage();
							const violations: string[] = [];
							const errors: string[] = [];
							await page.exposeFunction('setupCspViolation', (value: string) =>
								violations.push(value)
							);
							await page.addInitScript(() => {
								document.addEventListener('securitypolicyviolation', (event) => {
									void (
										window as unknown as { setupCspViolation(value: string): Promise<void> }
									).setupCspViolation(event.effectiveDirective);
								});
							});
							page.on('pageerror', (error) => errors.push(error.message));
							const response = await page.goto(`${origin}setup`);
							expect(response?.status()).toBe(200);
							expect(response?.headers()['content-security-policy']).toContain("script-src 'self'");
							await expect(
								page.getByRole('heading', { name: 'Create the first admin' })
							).toBeVisible();
							await inspect(page);
							const submit = async () => {
								if (input === 'keyboard') await page.getByLabel('Setup token').press('Enter');
								else await page.getByRole('button', { name: 'Create admin' }).click();
							};
							await submit();
							await expect(page.getByLabel('Setup token')).toBeFocused();
							await expect(page.getByText('Paste the setup token.')).toBeVisible();
							await inspect(page);
							await fill(page);
							let release: () => void = () => {};
							const pending = new Promise<void>((resolve) => {
								release = resolve;
							});
							await page.route('**/api/v1/setup', async (route) => {
								await pending;
								await route.fulfill({
									status: 401,
									contentType: 'application/problem+json',
									body: JSON.stringify({
										status: 401,
										title: 'Unauthorized',
										code: 'setup_credential_invalid'
									})
								});
							});
							await submit();
							await expect(page.getByRole('button', { name: 'Create admin' })).toBeDisabled();
							await expect(page.getByRole('status')).toHaveText('Creating the admin…');
							await inspect(page);
							release();
							await expect(page.getByRole('status')).toContainText(
								'That setup token is not valid.'
							);
							await expect(page.getByLabel('Password', { exact: true })).toHaveValue('');
							await expect(page.getByLabel('Username')).toHaveValue('owner');
							await inspect(page);
							await page.unroute('**/api/v1/setup');
							await page.route('**/api/v1/setup', (route) =>
								route.fulfill({
									status: 201,
									contentType: 'application/json',
									body: JSON.stringify({
										user: {
											id: 1,
											username: 'owner',
											role: 'admin',
											disabled: false,
											revision: 1,
											createdAt: '2026-10-23T12:00:00Z'
										}
									})
								})
							);
							await fill(page);
							await submit();
							await expect(page.getByRole('heading', { name: 'Admin created' })).toBeVisible();
							await inspect(page);
							await page.unroute('**/api/v1/setup');
							await page.route('**/api/v1/setup', (route) =>
								route.fulfill({
									status: 409,
									contentType: 'application/problem+json',
									body: JSON.stringify({ status: 409, title: 'Conflict', code: 'setup_completed' })
								})
							);
							await page.reload();
							await fill(page);
							await submit();
							await expect(
								page.getByRole('heading', { name: 'Setup is already complete' })
							).toBeVisible();
							await expect(page.locator('form')).toHaveCount(0);
							await inspect(page);
							await page
								.getByRole('heading', { name: 'Setup is already complete' })
								.evaluate((element) => {
									element.textContent =
										'[Šéţûþ îš åļŕéåðý çöɱþļéţé — expanded translation with a long explanation]';
								});
							await inspect(page);
							expect(errors).toEqual([]);
							expect(violations).toEqual([]);
						} finally {
							await context.close();
						}
					}
				}
			}
		}
	});

	test(`${base}: real setup creates one admin and refuses replay`, async ({ page }) => {
		expect(origin).toBeTruthy();
		await page.goto(`${origin}setup`);
		await fill(page, 'wrong-credential');
		await page.getByRole('button', { name: 'Create admin' }).click();
		await expect(page.getByRole('status')).toContainText('That setup token is not valid.');
		await fill(page);
		await page.getByLabel('Username').fill('hidden' + String.fromCodePoint(0x202e) + 'format');
		const rejected = page.waitForResponse(
			(response) => response.url().endsWith('/api/v1/setup') && response.status() === 422
		);
		await page.getByRole('button', { name: 'Create admin' }).click();
		const problem = await (await rejected).json();
		expect(problem.errors).toEqual([
			{ location: 'body.username', code: 'invalid', message: 'invalid value' }
		]);
		const username = page.getByLabel('Username');
		await expect(username).toBeFocused();
		await expect(username).toHaveAttribute('aria-describedby', 'setup-username-error');
		await expect(
			page.getByText('Use 1 to 64 characters without control or formatting characters.')
		).toBeVisible();
		await page.getByLabel('Password', { exact: true }).fill(password);
		await expect(username).toHaveAttribute('aria-invalid', 'true');
		await expect(
			page.getByText('Use 1 to 64 characters without control or formatting characters.')
		).toBeVisible();
		await username.fill('owner');
		await expect(username).not.toHaveAttribute('aria-invalid', 'true');
		await page.getByRole('button', { name: 'Create admin' }).click();
		await expect(page.getByRole('heading', { name: 'Admin created' })).toBeVisible();
		await expect(page.getByRole('status')).toContainText('owner is the admin');
		await page.getByRole('link', { name: 'Open Boxset' }).focus();
		await page.keyboard.press('Enter');
		await expect(page).toHaveURL(origin!);
		await page.goto(`${origin}setup`);
		await page.reload();
		await fill(page);
		await page.getByRole('button', { name: 'Create admin' }).click();
		await expect(page.getByRole('heading', { name: 'Setup is already complete' })).toBeVisible();
		await page.getByRole('link', { name: 'Open Boxset' }).focus();
		await page.keyboard.press('Enter');
		await expect(page).toHaveURL(origin!);
	});
}
