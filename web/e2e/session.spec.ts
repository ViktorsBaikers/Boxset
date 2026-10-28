import AxeBuilder from '@axe-core/playwright';
import { expect, request, test, type Page } from '@playwright/test';
import { readFileSync } from 'node:fs';

const mode = process.env.BOXSET_SESSION_MODE;
const password = 'correct horse battery staple';

function fixtureContract(raw: string | undefined) {
	if (!raw || new TextEncoder().encode(raw).length > 512)
		throw new Error('fixture descriptor invalid');
	const value = JSON.parse(raw);
	const keys = [...raw.matchAll(/("(?:\\.|[^"\\])*")\s*:/g)].map((entry) => JSON.parse(entry[1]));
	if (
		!value ||
		Array.isArray(value) ||
		typeof value !== 'object' ||
		keys.length !== 5 ||
		new Set(keys).size !== 5 ||
		Object.keys(value).sort().join(',') !==
			'afterHeader,beforeHeader,operations,transitionHeader,version' ||
		!/"version"\s*:\s*1\s*[,}]/.test(raw) ||
		value.version !== 1 ||
		!Number.isInteger(value.version) ||
		value.transitionHeader !== 'X-Boxset-Test-Role-Transition' ||
		value.beforeHeader !== 'X-Boxset-Test-Revision-Before' ||
		value.afterHeader !== 'X-Boxset-Test-Revision-After' ||
		JSON.stringify(value.operations) !== '["restrict","restore"]'
	)
		throw new Error('fixture descriptor invalid');
	return value as {
		version: number;
		transitionHeader: string;
		beforeHeader: string;
		afterHeader: string;
		operations: ['restrict', 'restore'];
	};
}

async function bounded<T>(work: Promise<T>, boundary: string): Promise<T> {
	let timer: ReturnType<typeof setTimeout>;
	try {
		return await Promise.race([
			work,
			new Promise<never>((_, reject) => {
				timer = setTimeout(() => reject(new Error(boundary)), 5000);
			})
		]);
	} finally {
		clearTimeout(timer!);
	}
}

type CurrentSession = {
	user: { id: number; role: string; revision: number };
	rights: string[];
	credential: string;
	restoreEpoch: number;
};

test.beforeEach(async ({ page }) => {
	await page.addInitScript(() => {
		Object.defineProperty(navigator, 'onLine', { configurable: true, get: () => true });
	});
});

async function fill(page: Page, value = password) {
	await page.getByLabel('Username').fill('owner');
	await page.getByLabel('Password', { exact: true }).fill(value);
}

async function inspect(page: Page) {
	expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
	const scan = await new AxeBuilder({ page })
		.withTags(['wcag2a', 'wcag2aa', 'wcag21aa', 'wcag22aa'])
		.analyze();
	expect(scan.violations).toEqual([]);
}

async function visibility(page: Page, visible: boolean) {
	await page.evaluate((visible) => {
		Object.defineProperty(document, 'visibilityState', {
			configurable: true,
			get: () => (visible ? 'visible' : 'hidden')
		});
		document.dispatchEvent(new Event('visibilitychange'));
	}, visible);
}

async function connectivity(page: Page, online: boolean) {
	await page.evaluate((online) => {
		Object.defineProperty(navigator, 'onLine', { configurable: true, get: () => online });
		window.dispatchEvent(new Event(online ? 'online' : 'offline'));
	}, online);
}

if (mode === 'states') {
	const entries = readFileSync('/proc/self/environ', 'utf8')
		.split('\0')
		.filter((entry) => entry.startsWith('BOXSET_SESSION_FIXTURE_CONTRACT='));
	const envelope = (values: string[]) => {
		if (values.length !== 1) throw new Error('fixture descriptor environment invalid');
		return fixtureContract(values[0].slice('BOXSET_SESSION_FIXTURE_CONTRACT='.length));
	};
	expect(() => envelope([])).toThrow();
	expect(() => envelope([...entries, ...entries])).toThrow();
	const raw = process.env.BOXSET_SESSION_FIXTURE_CONTRACT;
	envelope(entries);
	const descriptor = fixtureContract(raw);
	for (const invalid of [
		undefined,
		'',
		'{}',
		'null',
		raw! + raw!,
		' '.repeat(513) + raw!,
		raw!.replace('"version":1', '"version":2'),
		raw!.replace('"version":1', '"Version":1'),
		raw!.replace('"version":1', '"version":1.0'),
		raw!.replace('"version":1', '"version":"1"'),
		raw!.replace('"version":1', '"version":1,"version":1'),
		raw!.replace('"version":1', '"version":1,"vers\\u0069on":1'),
		raw!.replace('"version":1,', ''),
		raw!.replace('"version":1', '"unknown":0,"version":1'),
		raw!.replace('["restrict","restore"]', '["restore","restrict"]'),
		raw!.replace('"beforeHeader":"X-Boxset-Test-Revision-Before"', '"beforeHeader":null')
	]) {
		expect(
			() => fixtureContract(invalid),
			'fixture descriptor refusal before navigation'
		).toThrow();
	}
	const targets = [process.env.S5_DEV_URL, process.env.S5_PREVIEW_URL];
	for (const [index, target] of targets.entries()) {
		if (!target) throw new Error('fixture installation missing');
		const url = new URL(target);
		if (
			url.protocol !== 'http:' ||
			url.hostname !== '127.0.0.1' ||
			!url.port ||
			url.username ||
			url.password ||
			url.search ||
			url.hash ||
			url.pathname !== (index === 0 ? '/' : '/media/') ||
			url.href !== target
		)
			throw new Error('fixture installation invalid');
	}
	if (targets[0] === targets[1]) throw new Error('fixture installations duplicated');
	for (const [base, origin] of Object.entries({
		root: process.env.S5_DEV_URL,
		media: process.env.S5_PREVIEW_URL
	})) {
		test(`${base}: real sign-in, current session, revocation and sign-out`, async ({ page }) => {
			expect(origin).toBeTruthy();
			const sessionURL = new URL('api/v1/session', origin!).href;
			const pageSession = (candidate: import('@playwright/test').Request) =>
				candidate.method() === 'GET' &&
				candidate.url() === sessionURL &&
				candidate.frame() === page.mainFrame() &&
				candidate.resourceType() === 'fetch';
			let pageRequests = 0;
			page.on('request', (candidate) => {
				if (
					candidate.url() === sessionURL &&
					candidate.method() === 'GET' &&
					pageSession(candidate)
				)
					pageRequests++;
			});
			await page.goto(origin!);
			await expect(page.getByRole('status')).toHaveText('Sign in to check this Boxset.');
			await page.getByRole('link', { name: 'Sign in', exact: true }).click();
			await page.getByRole('button', { name: 'Sign in', exact: true }).click();
			await expect(page.getByLabel('Username')).toBeFocused();
			await expect(page.getByLabel('Username')).toHaveAttribute('aria-invalid', 'true');
			await fill(page, 'wrong synthetic password');
			await page.getByLabel('Password', { exact: true }).press('Enter');
			await expect(page.getByRole('status')).toHaveText(
				'The username or password is incorrect. Try again.'
			);
			await expect(page.getByLabel('Password', { exact: true })).toHaveValue('');
			await expect(page.getByLabel('Username')).toHaveValue('owner');
			await fill(page);
			const initialPageSession = page.waitForResponse(
				(response) => response.status() === 200 && pageSession(response.request()),
				{ timeout: 5000 }
			);
			void initialPageSession.catch(() => {});
			await page.getByRole('button', { name: 'Sign in', exact: true }).click();
			await expect(page.getByRole('status')).toHaveText('Boxset is ready.');
			await expect(page.getByText('owner', { exact: true })).toBeVisible();
			const current = await page.request.get(new URL('api/v1/session', origin!).href);
			expect(current.status()).toBe(200);
			expect((await current.json()).rights).toEqual(['status:read', 'users:read', 'users:write']);
			await inspect(page);
			await page.evaluate(() => {
				document.documentElement.dataset.sessionDocument = 'retained';
			});
			const acquired = await bounded(initialPageSession, 'FRESHNESS_INITIAL_QUERY');
			expect(acquired.headers()['cache-control'], 'FRESHNESS_INITIAL_NO_STORE').toBe('no-store');
			let prior: CurrentSession = await acquired.json();
			expect(prior.user.role).toBe('admin');
			expect(prior.rights).toEqual(['status:read', 'users:read', 'users:write']);
			expect(pageRequests, 'FRESHNESS_PRIOR_QUERY').toBeGreaterThan(0);
			console.log(
				'SESSION-FRESHNESS ' +
					JSON.stringify({
						phase: 'initial',
						fetched: true,
						status: 200,
						noStore: true,
						restricted: false
					})
			);
			async function confirmation(operation: 'restrict' | 'restore' | 'revoke') {
				const oldRequests = pageRequests;
				let release!: () => void;
				let observed!: () => void;
				const held = new Promise<void>((resolve) => {
					release = resolve;
				});
				const fetched = new Promise<void>((resolve) => {
					observed = resolve;
				});
				let genuine: CurrentSession | undefined;
				let seen = 0;
				const delivered = page.waitForResponse((response) => pageSession(response.request()), {
					timeout: 5000
				});
				void delivered.catch(() => {});
				await page.route(sessionURL, async (route) => {
					expect(pageSession(route.request()), 'FRESHNESS_REQUEST_IDENTITY').toBe(true);
					seen++;
					expect(seen, 'FRESHNESS_SINGLE_FETCH').toBe(1);
					const headers = { ...route.request().headers() };
					if (operation !== 'revoke') headers[descriptor.transitionHeader] = operation;
					const response = await route.fetch({
						headers,
						timeout: 5000,
						maxRedirects: 0,
						maxRetries: 0
					});
					expect(response.headers()['cache-control'], 'FRESHNESS_NO_STORE').toBe('no-store');
					expect(response.status(), 'FRESHNESS_HTTP_STATUS').toBe(
						operation === 'revoke' ? 401 : 200
					);
					const body = await response.json();
					if (operation === 'revoke')
						expect(body.code, 'FRESHNESS_REAL_REVOCATION').toBe('credential_invalid');
					else {
						const revision = (name: string) => {
							const values = response
								.headersArray()
								.filter((header) => header.name.toLowerCase() === name.toLowerCase())
								.map((header) => header.value);
							expect(values.length, 'FRESHNESS_ACK_UNIQUE').toBe(1);
							expect(values[0], 'FRESHNESS_ACK_INTEGER').toMatch(/^[1-9][0-9]*$/);
							const number = Number(values[0]);
							expect(Number.isSafeInteger(number) && number > 0, 'FRESHNESS_ACK_BOUND').toBe(true);
							return number;
						};
						const before = revision(descriptor.beforeHeader),
							after = revision(descriptor.afterHeader);
						expect(before, 'FRESHNESS_BEFORE_REVISION').toBe(prior.user.revision);
						expect(after, 'FRESHNESS_AFTER_REVISION').toBe(before + 1);
						expect(body.user.id, 'FRESHNESS_SAME_USER').toBe(prior.user.id);
						expect(body.user.revision, 'FRESHNESS_CURRENT_REVISION').toBe(after);
						expect(body.user.role, 'FRESHNESS_CURRENT_ROLE').toBe(
							operation === 'restrict' ? 'viewer' : 'admin'
						);
						expect(body.rights, 'FRESHNESS_CURRENT_RIGHTS').toEqual(
							operation === 'restrict'
								? ['status:read']
								: ['status:read', 'users:read', 'users:write']
						);
						expect(body.credential, 'FRESHNESS_SAME_CREDENTIAL').toBe('cookie');
						expect(body.restoreEpoch, 'FRESHNESS_SAME_EPOCH').toBe(prior.restoreEpoch);
						genuine = body;
					}
					console.log(
						'SESSION-FRESHNESS ' +
							JSON.stringify({
								phase: operation,
								fetched: true,
								status: response.status(),
								noStore: true,
								restricted: operation === 'restrict'
							})
					);
					observed();
					await bounded(held, 'FRESHNESS_DELIVERY_RELEASE');
					await route.fulfill({ response });
				});
				try {
					await visibility(page, false);
					await expect(page.getByText('owner', { exact: true })).toHaveCount(0);
					if (operation === 'revoke') {
						const revoker = await request.newContext({
							storageState: { cookies: await page.context().cookies(origin!), origins: [] }
						});
						try {
							expect(
								(
									await revoker.delete(sessionURL, {
										timeout: 5000,
										maxRetries: 0,
										maxRedirects: 0
									})
								).status()
							).toBe(204);
						} finally {
							await revoker.dispose();
						}
					}
					await visibility(page, true);
					await expect
						.poll(() => pageRequests, { timeout: 5000, message: 'FRESHNESS_SESSION_REQUEST' })
						.toBe(oldRequests + 1);
					await bounded(fetched, 'FRESHNESS_RESPONSE_FETCH');
					await expect(page.getByRole('status'), 'FRESHNESS_PUBLIC_PENDING').toHaveText(
						'Loading Boxset…'
					);
					await expect(
						page.getByText('owner', { exact: true }),
						'FRESHNESS_NO_PRIVATE_PAINT'
					).toHaveCount(0);
					await expect(
						page.getByText('Boxset is ready.', { exact: true }),
						'FRESHNESS_NO_PRIVATE_STATUS'
					).toHaveCount(0);
					expect(
						await page.evaluate(() => document.documentElement.dataset.sessionDocument),
						'FRESHNESS_RETAINED_DOCUMENT'
					).toBe('retained');
					release();
					const actual = await bounded(delivered, 'FRESHNESS_PAGE_RESPONSE');
					expect(actual.headers()['cache-control'], 'FRESHNESS_PAGE_NO_STORE').toBe('no-store');
					expect(actual.status(), 'FRESHNESS_PAGE_STATUS').toBe(operation === 'revoke' ? 401 : 200);
					if (genuine) {
						expect(await actual.json(), 'FRESHNESS_GENUINE_PAGE_BODY').toEqual(genuine);
						prior = genuine;
					} else
						expect((await actual.json()).code, 'FRESHNESS_PAGE_REVOCATION').toBe(
							'credential_invalid'
						);
					await expect(page.getByRole('status')).toHaveText(
						operation === 'revoke' ? 'Sign in to check this Boxset.' : 'Boxset is ready.'
					);
				} finally {
					release();
					await page.unroute(sessionURL);
				}
			}
			await confirmation('restrict');
			await confirmation('restore');
			const healthy = await page.request.get(sessionURL);
			expect(healthy.status()).toBe(200);
			expect((await healthy.json()).rights).toEqual(['status:read', 'users:read', 'users:write']);
			expect(healthy.headers()[descriptor.afterHeader.toLowerCase()]).toBeUndefined();
			await confirmation('revoke');
			await page.getByRole('link', { name: 'Sign in', exact: true }).click();
			await fill(page);
			await page.getByRole('button', { name: 'Sign in', exact: true }).click();
			await expect(page.getByRole('status')).toHaveText('Boxset is ready.');
			await page.evaluate(() => {
				document.documentElement.dataset.sessionDocument = 'retained';
			});
			for (const transition of ['visibility', 'offline']) {
				let release!: () => void;
				let observed!: () => void;
				const held = new Promise<void>((resolve) => {
					release = resolve;
				});
				const requested = new Promise<void>((resolve) => {
					observed = resolve;
				});
				await page.route('**/ping', async (route) => {
					const response = await route.fetch({ timeout: 5000, maxRedirects: 0, maxRetries: 0 });
					expect(response.status()).toBe(200);
					expect(response.headers()['cache-control']).toBe('no-store');
					observed();
					await bounded(held, 'FRESHNESS_LIFECYCLE_RELEASE');
					await route.fulfill({ response });
				});
				try {
					if (transition === 'visibility') await visibility(page, false);
					else await connectivity(page, false);
					await expect(page.getByText('owner', { exact: true })).toHaveCount(0);
					if (transition === 'offline') {
						const revoked = await page.request.delete(new URL('api/v1/session', origin!).href);
						expect(revoked.status()).toBe(204);
					}
					if (transition === 'visibility') await visibility(page, true);
					else {
						await page.evaluate(() => {
							Object.defineProperty(navigator, 'onLine', { configurable: true, get: () => true });
						});
						await page.getByRole('button', { name: 'Retry' }).click();
					}
					await expect(page.getByRole('status')).toHaveText('Loading Boxset…');
					await expect(page.getByText('owner', { exact: true })).toHaveCount(0);
					await bounded(requested, 'FRESHNESS_LIFECYCLE_REQUEST');
					expect(await page.evaluate(() => document.documentElement.dataset.sessionDocument)).toBe(
						'retained'
					);
					release();
					await expect(page.getByRole('status')).toHaveText(
						transition === 'visibility' ? 'Boxset is ready.' : 'Sign in to check this Boxset.'
					);
				} finally {
					release();
					await page.unroute('**/ping');
				}
			}
			await expect(page.getByRole('status')).toHaveText('Sign in to check this Boxset.');
			await expect(page.getByText('owner', { exact: true })).toHaveCount(0);
			await page.getByRole('link', { name: 'Sign in', exact: true }).click();
			await fill(page);
			await page.getByRole('button', { name: 'Sign in', exact: true }).click();
			await expect(page.getByRole('status')).toHaveText('Boxset is ready.');
			// A transport failure does not end server authority, but must hide private paint.
			await page.route('**/api/v1/session', async (route) => {
				if (route.request().method() === 'DELETE') await route.abort('connectionfailed');
				else await route.continue();
			});
			await page.getByRole('button', { name: 'Sign out' }).click();
			await expect(page.getByRole('status')).toHaveText(
				'Sign-out could not be confirmed. Retry to end this session.'
			);
			await expect(page.getByText('owner', { exact: true })).toHaveCount(0);
			expect((await page.request.get(new URL('api/v1/session', origin!).href)).status()).toBe(200);
			await page.unroute('**/api/v1/session');
			await page.getByRole('button', { name: 'Sign out' }).click();
			await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
			expect((await page.request.get(new URL('api/v1/session', origin!).href)).status()).toBe(401);
			for (const outcome of ['lost', 'revoked']) {
				await fill(page);
				await page.getByRole('button', { name: 'Sign in', exact: true }).click();
				await expect(page.getByRole('status')).toHaveText('Boxset is ready.');
				const credential = (await page.context().cookies(origin!)).filter((cookie) =>
					cookie.name.startsWith('boxset-')
				);
				expect(credential.length).toBe(1);
				const credentialRetained = async () =>
					(await page.context().cookies(origin!)).some(
						(cookie) => cookie.name === credential[0].name && cookie.value === credential[0].value
					);
				// Revocation must not deliver its deletion cookie into the browser's jar.
				const revoker = await request.newContext({
					storageState: { cookies: credential, origins: [] }
				});
				try {
					if (outcome === 'revoked') {
						expect((await revoker.delete(new URL('api/v1/session', origin!).href)).status()).toBe(
							204
						);
						expect(
							await credentialRetained(),
							'server-side revocation preserves the browser credential'
						).toBe(true);
					} else {
						await page.route('**/api/v1/session', async (route) => {
							if (route.request().method() === 'DELETE') {
								expect(
									(await revoker.delete(new URL('api/v1/session', origin!).href)).status()
								).toBe(204);
								expect(
									await credentialRetained(),
									'lost response preserves the browser credential'
								).toBe(true);
								await route.abort('connectionfailed');
							} else await route.continue();
						});
						await page.getByRole('button', { name: 'Sign out' }).click();
						await expect(page.getByRole('status')).toHaveText(
							'Sign-out could not be confirmed. Retry to end this session.'
						);
						await expect(page.getByText('owner', { exact: true })).toHaveCount(0);
						await page.unroute('**/api/v1/session');
					}
					expect(await credentialRetained(), 'retry sends the original revoked credential').toBe(
						true
					);
					const denied = page.waitForResponse(
						(response) =>
							response.request().method() === 'DELETE' &&
							new URL(response.url()).pathname.endsWith('/api/v1/session')
					);
					await page.getByRole('button', { name: 'Sign out' }).click();
					const response = await denied;
					expect(response.status()).toBe(401);
					expect((await response.json()).code).toBe('credential_invalid');
					await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
					expect(
						(await page.request.get(new URL('api/v1/system/status', origin!).href)).status()
					).toBe(401);
				} finally {
					try {
						await page.unroute('**/api/v1/session');
					} finally {
						await revoker.dispose();
					}
				}
			}
			await page.getByRole('link', { name: 'First access? Create the admin' }).click();
			await expect(page.getByRole('heading', { name: 'Create the first admin' })).toBeVisible();
		});

		test(`${base}: public pending, lifecycle, offline and denied states reflow under CSP`, async ({
			browser
		}) => {
			test.setTimeout(240000);
			for (const width of [320, 768, 1024, 1440]) {
				let darkGround = '';
				for (const theme of ['dark', 'light'] as const) {
					for (const reducedMotion of ['reduce', 'no-preference'] as const) {
						for (const input of ['keyboard', 'pointer']) {
							const context = await browser.newContext({
								viewport: { width, height: 1000 },
								colorScheme: theme,
								reducedMotion
							});
							try {
								const page = await context.newPage();
								const violations: string[] = [];
								const errors: string[] = [];
								await page.exposeFunction('sessionCsp', (directive: string) =>
									violations.push(directive)
								);
								await page.addInitScript((theme) => {
									const selectTheme = () => {
										if (!document.documentElement) return false;
										document.documentElement.dataset.theme = theme;
										return true;
									};
									if (!selectTheme()) {
										const root = new MutationObserver(() => {
											if (selectTheme()) root.disconnect();
										});
										root.observe(document, { childList: true });
									}
									Object.defineProperty(navigator, 'onLine', {
										configurable: true,
										get: () => true
									});
									document.addEventListener('securitypolicyviolation', (event) => {
										void (
											window as unknown as { sessionCsp(value: string): Promise<void> }
										).sessionCsp(event.effectiveDirective);
									});
								}, theme);
								page.on('pageerror', (error) => errors.push(error.message));
								const response = await page.goto(new URL('sign-in', origin!).href);
								// Discriminate actual token selection from the browser preference.
								const scheme = await page.evaluate(
									() => getComputedStyle(document.documentElement).colorScheme
								);
								expect(scheme, 'session matrix uses the requested computed token scheme').toBe(
									theme
								);
								const ground = await page
									.locator('body')
									.evaluate((element) => getComputedStyle(element).backgroundColor);
								if (theme === 'dark') darkGround = ground;
								else {
									expect(darkGround).not.toBe('');
									expect(ground).not.toBe(darkGround);
								}
								expect(response?.headers()['content-security-policy']).toContain(
									"script-src 'self'"
								);
								const submit = async () => {
									if (input === 'keyboard')
										await page.getByLabel('Password', { exact: true }).press('Enter');
									else await page.getByRole('button', { name: 'Sign in', exact: true }).click();
								};
								await submit();
								await expect(page.getByLabel('Username')).toBeFocused();
								await expect(page.getByLabel('Username')).toHaveAttribute('aria-invalid', 'true');
								await inspect(page);
								await fill(page);
								let release!: () => void;
								const held = new Promise<void>((resolve) => {
									release = resolve;
								});
								await page.route('**/api/v1/session', async (route) => {
									await held;
									await route.fulfill({
										status: 401,
										contentType: 'application/problem+json',
										body: JSON.stringify({ status: 401, code: 'credentials_invalid' })
									});
								});
								await submit();
								await expect(page.getByRole('status')).toHaveText('Signing in…');
								await expect(
									page.getByRole('button', { name: 'Sign in', exact: true })
								).toBeDisabled();
								await inspect(page);
								release();
								await expect(page.getByRole('status')).toHaveText(
									'The username or password is incorrect. Try again.'
								);
								await expect(page.getByLabel('Password', { exact: true })).toHaveValue('');
								await expect(page.getByLabel('Username')).toHaveValue('owner');
								await inspect(page);
								await page.unroute('**/api/v1/session');
								await fill(page);
								await submit();
								await expect(page.getByRole('status')).toHaveText('Boxset is ready.');
								await inspect(page);
								await page.route('**/api/v1/system/status', (route) =>
									route.fulfill({
										status: 403,
										contentType: 'application/problem+json',
										body: JSON.stringify({ status: 403, code: 'forbidden' })
									})
								);
								await page.reload();
								await expect(page.getByRole('status')).toHaveText(
									'You do not have permission to view this information.'
								);
								await expect(page.getByText('owner', { exact: true })).toHaveCount(0);
								await inspect(page);
								for (const [state, message] of [
									['starting', 'Boxset is starting. Wait a moment, then retry.'],
									['recovering', 'Boxset is recovering. Check the server before trying again.'],
									['draining', 'Boxset is stopping. Wait for it to restart before signing in.']
								]) {
									await page.route('**/ping', (route) =>
										route.fulfill({
											status: 200,
											headers: { 'X-Boxset-State': state, 'Cache-Control': 'no-store' },
											body: 'ok'
										})
									);
									await page.reload();
									await expect(page.getByRole('status')).toHaveText(message);
									await inspect(page);
									await page.unroute('**/ping');
								}
								await page.evaluate(() => {
									Object.defineProperty(navigator, 'onLine', {
										configurable: true,
										get: () => false
									});
									window.dispatchEvent(new Event('offline'));
								});
								await expect(page.getByRole('heading', { name: 'You are offline' })).toBeVisible();
								await page.getByRole('button', { name: 'Retry' }).focus();
								if (input === 'keyboard') await page.keyboard.press('Enter');
								else await page.getByRole('button', { name: 'Retry' }).click();
								await inspect(page);
								await page.evaluate(() => {
									Object.defineProperty(navigator, 'onLine', {
										configurable: true,
										get: () => true
									});
									window.dispatchEvent(new Event('online'));
								});
								await expect(page.getByRole('status')).toHaveText(
									'You do not have permission to view this information.'
								);
								await page.getByRole('status').evaluate((element) => {
									const span = element.querySelector('span')!;
									span.textContent =
										'[Ýöü dö nöt hávë përmïssïön tö vïëw thïs ïnförmátïön. Expanded translation for a longer explanation.]';
									span.style.lineHeight = '1.5';
									span.style.letterSpacing = '0.12em';
									span.style.wordSpacing = '0.16em';
								});
								await inspect(page);
								await page.unroute('**/api/v1/system/status');
								await page.reload();
								await expect(page.getByRole('status')).toHaveText('Boxset is ready.');
								if (input === 'keyboard')
									await page.getByRole('button', { name: 'Sign out' }).press('Enter');
								else await page.getByRole('button', { name: 'Sign out' }).click();
								await expect(
									page.getByRole('heading', { name: 'Sign in', exact: true })
								).toBeVisible();
								await page.goto(origin!);
								await expect(page.getByRole('status')).toHaveText('Sign in to check this Boxset.');
								await expect(page.getByText('owner', { exact: true })).toHaveCount(0);
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
	}
} else if (mode === 'two-installations') {
	for (const [scheme, origin] of Object.entries({
		http: process.env.BOXSET_SESSION_HTTP,
		https: process.env.BOXSET_SESSION_TLS
	})) {
		test(`${scheme}: two actual CLI installations keep independent cookies and logout`, async ({
			browser
		}) => {
			expect(origin).toBeTruthy();
			const context = await browser.newContext({ ignoreHTTPSErrors: true });
			await context.addInitScript(() => {
				Object.defineProperty(navigator, 'onLine', { configurable: true, get: () => true });
			});
			try {
				const a = await context.newPage();
				const b = await context.newPage();
				for (const [page, base] of [
					[a, '/a'],
					[b, '/b']
				] as const) {
					await page.goto(`${origin}${base}/sign-in`);
					await fill(page);
					await page.getByRole('button', { name: 'Sign in', exact: true }).click();
					await expect(page.getByRole('status')).toHaveText('Boxset is ready.');
				}
				const cookies = (await context.cookies()).filter((cookie) =>
					cookie.name.includes('boxset-')
				);
				expect(cookies.length).toBe(2);
				expect(new Set(cookies.map((cookie) => cookie.name)).size).toBe(2);
				expect(
					cookies.every(
						(cookie) =>
							cookie.httpOnly && cookie.sameSite === 'Lax' && cookie.secure === (scheme === 'https')
					)
				).toBe(true);
				expect(
					cookies.every((cookie) =>
						scheme === 'https'
							? cookie.name.startsWith('__Host-boxset-') && cookie.path === '/'
							: cookie.name.startsWith('boxset-') && ['/a', '/b'].includes(cookie.path)
					)
				).toBe(true);
				await a.reload();
				await expect(a.getByRole('status')).toHaveText('Boxset is ready.');
				await a.getByRole('button', { name: 'Sign out' }).click();
				await expect(a.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
				expect((await context.request.get(`${origin}/a/api/v1/session`)).status()).toBe(401);
				await b.reload();
				await expect(b.getByRole('status')).toHaveText('Boxset is ready.');
				expect((await context.request.get(`${origin}/b/api/v1/system/status`)).status()).toBe(200);
				await b.getByRole('button', { name: 'Sign out' }).click();
				await expect(b.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
				expect((await context.request.get(`${origin}/b/api/v1/session`)).status()).toBe(401);
			} finally {
				await context.close();
			}
		});
	}
} else if (mode === 'local-access') {
	for (const [base, origin] of Object.entries({
		root: process.env.S5_DEV_URL,
		media: process.env.S5_PREVIEW_URL
	})) {
		test(`${base}: direct local access warns, hides identity and revokes a signed-in cookie`, async ({
			page,
			context
		}) => {
			expect(origin).toBeTruthy();
			const sessionURL = new URL('api/v1/session', origin!).href;
			const warning = page.getByRole('heading', {
				name: 'This network is trusted without signing in'
			});
			const localAccess = page.getByText('Local access without sign-in', { exact: true });
			const confirmed = async () => {
				const response = await page.request.get(sessionURL);
				expect(response.status()).toBe(200);
				return response.json();
			};
			await page.goto(origin!);
			await expect(page.getByRole('status')).toHaveText('Boxset is ready.');
			await expect(warning).toBeVisible();
			await expect(localAccess).toBeVisible();
			await expect(page.getByText('owner', { exact: true })).toHaveCount(0);
			await expect(page.getByRole('button', { name: 'Sign out' })).toHaveCount(0);
			await expect(page.getByRole('link', { name: 'Sign in', exact: true })).toBeVisible();
			const grant = await confirmed();
			expect(grant.credential).toBe('local_bypass');
			expect(grant.localBypassEnabled).toBe(true);
			expect(grant.canLogout).toBe(false);
			expect(grant.user).toBeUndefined();
			expect(grant.expiresAt).toBeUndefined();
			expect(grant.rights).toEqual(['status:read', 'users:read', 'users:write']);
			await inspect(page);
			let release!: () => void;
			const held = new Promise<void>((resolve) => {
				release = resolve;
			});
			await page.route(sessionURL, async (route) => {
				if (route.request().method() !== 'GET') return route.continue();
				await held;
				await route.continue();
			});
			await page.evaluate(() => window.dispatchEvent(new Event('online')));
			await expect(page.getByRole('status')).toHaveText('Loading Boxset…');
			await expect(warning).toBeVisible();
			await expect(localAccess).toHaveCount(0);
			await expect(page.getByText('owner', { exact: true })).toHaveCount(0);
			release();
			// Unroute only once the barrier delivered its response: removing a route whose
			// handler is still suspended continues that request itself.
			await expect(page.getByRole('status')).toHaveText('Boxset is ready.');
			await expect(localAccess).toBeVisible();
			await page.unroute(sessionURL);
			await page.evaluate(() => {
				Object.defineProperty(navigator, 'onLine', { configurable: true, get: () => false });
				window.dispatchEvent(new Event('offline'));
			});
			await expect(page.getByRole('heading', { name: 'You are offline' })).toBeVisible();
			await expect(warning).toBeVisible();
			await expect(localAccess).toHaveCount(0);
			await page.evaluate(() => {
				Object.defineProperty(navigator, 'onLine', { configurable: true, get: () => true });
				window.dispatchEvent(new Event('online'));
			});
			await expect(page.getByRole('status')).toHaveText('Boxset is ready.');
			await page.getByRole('link', { name: 'Sign in', exact: true }).click();
			await fill(page);
			await page.getByRole('button', { name: 'Sign in', exact: true }).click();
			await expect(page.getByRole('status')).toHaveText('Boxset is ready.');
			await expect(page.getByRole('button', { name: 'Sign out' })).toBeVisible();
			await expect(warning).toBeVisible();
			const signedIn = await confirmed();
			expect(signedIn.credential).toBe('local_bypass');
			expect(signedIn.canLogout).toBe(true);
			expect(signedIn.user).toBeUndefined();
			expect(signedIn.expiresAt).toBeUndefined();
			const [cookie] = (await context.cookies(origin!)).filter((entry) =>
				entry.name.includes('boxset-')
			);
			expect(cookie).toBeTruthy();
			await page.route(sessionURL, async (route) => {
				if (route.request().method() === 'DELETE') await route.abort('connectionfailed');
				else await route.continue();
			});
			await page.getByRole('button', { name: 'Sign out' }).click();
			await expect(page.getByRole('status')).toHaveText(
				'Sign-out could not be confirmed. Retry to end this session.'
			);
			await expect(warning).toBeVisible();
			await expect(page.getByText('owner', { exact: true })).toHaveCount(0);
			await page.unroute(sessionURL);
			await page.getByRole('button', { name: 'Sign out' }).click();
			await expect(page.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
			expect(
				(await context.cookies(origin!)).filter((entry) => entry.name.includes('boxset-'))
			).toHaveLength(0);
			const replay = await request.newContext({
				baseURL: origin!,
				extraHTTPHeaders: { cookie: `${cookie.name}=${cookie.value}` }
			});
			try {
				const revoked = await replay.get(sessionURL);
				expect(revoked.status()).toBe(200);
				const body = await revoked.json();
				expect(body.credential).toBe('local_bypass');
				expect(body.canLogout).toBe(false);
				expect(body.user).toBeUndefined();
			} finally {
				await replay.dispose();
			}
			await page.goto(origin!);
			await expect(page.getByRole('status')).toHaveText('Boxset is ready.');
			await expect(warning).toBeVisible();
			await expect(localAccess).toBeVisible();
			expect((await confirmed()).canLogout).toBe(false);
			expect((await page.request.get(new URL('api/v1/system/status', origin!).href)).status()).toBe(
				200
			);
		});
	}
} else if (mode === 'recovery') {
	// Cold navigation in a real recovering installation: the data-free shell and
	// its hashed assets arrive under the production CSP, the public cue names the
	// state, protected data is denied, and the status snapshot is served only to a
	// current credential.
	for (const [base, tail] of [
		['root', 'DEV'],
		['media', 'PREVIEW']
	] as const) {
		for (const recovery of ['key_missing', 'restore_in_progress'] as const) {
			// Each recovery state has its own variables, so neither can shadow the other
			// through os.Getenv on the Go side: the fixture carries both.
			const prefix = `S5_${tail}_${recovery.toUpperCase()}`;
			test(`${base}: cold recovery navigation serves the public state and denies protected data (${recovery})`, async ({
				browser
			}) => {
				const origin = process.env[`${prefix}_URL`];
				const session = process.env[`${prefix}_SESSION`];
				expect(origin, 'recovering installation missing').toBeTruthy();
				if (!session || !/^[^=]+=\S+$/.test(session))
					throw new Error('recovery session cookie missing');
				const statusURL = new URL('api/v1/system/status', origin).href;
				const usersURL = new URL('api/v1/users', origin).href;
				const context = await browser.newContext();
				try {
					const violations: string[] = [];
					const errors: string[] = [];
					const assets: number[] = [];
					const page = await context.newPage();
					await page.exposeFunction('sessionCsp', (directive: string) =>
						violations.push(directive)
					);
					await page.addInitScript(() => {
						// The runner has no network, so the browser would otherwise report
						// itself offline and paint the reconnect cue instead of the state
						// under test.
						Object.defineProperty(navigator, 'onLine', { configurable: true, get: () => true });
						document.addEventListener('securitypolicyviolation', (event) => {
							void (window as unknown as { sessionCsp(value: string): Promise<void> }).sessionCsp(
								event.effectiveDirective
							);
						});
					});
					page.on('pageerror', (error) => errors.push(error.message));
					page.on('response', (response) => {
						if (new URL(response.url()).pathname.indexOf('/_app/immutable/') !== -1)
							assets.push(response.status());
					});
					// The credential this installation had before it broke.
					const [name, value] = session.split('=');
					await context.addCookies([{ name, value, url: origin }]);
					const response = await page.goto(origin!);
					expect(response?.status(), 'cold navigation serves the shell').toBe(200);
					await expect(page.getByRole('status')).toHaveText(
						'Boxset is recovering. Check the server before trying again.'
					);
					await expect(page.getByRole('button', { name: 'Retry' })).toBeVisible();
					// A data-free shell carries no private paint and no identity.
					await expect(page.getByText('owner', { exact: true })).toHaveCount(0);
					await expect(page.getByRole('button', { name: 'Sign out' })).toHaveCount(0);
					expect(assets.length, 'the shell loaded its hashed assets').toBeGreaterThan(0);
					expect(assets.every((status) => status === 200)).toBe(true);
					await inspect(page);
					// The served outcome of a credentialed read, which is what the lifecycle
					// contract states: key loss confines every authenticated request but the
					// status read, and a pending restore serves no credential at all.
					const status = await page.request.get(statusURL);
					const users = await page.request.get(usersURL);
					if (recovery === 'key_missing') {
						expect(status.status(), 'current credential status').toBe(200);
						expect((await status.json()).reason).toBe('key_missing');
						expect(users.status(), 'authenticated read in key loss').toBe(503);
						expect((await users.json()).code).toBe('recovering');
						expect(users.headers()['retry-after']).toBeTruthy();
					} else {
						expect(status.status()).toBe(401);
						expect((await status.json()).code).toBe('unauthenticated');
						expect(users.status()).toBe(401);
						expect((await users.json()).code).toBe('unauthenticated');
					}
					// The unauthenticated control: no cookie, no credential, always 401.
					const anonymous = await request.newContext();
					try {
						for (const url of [statusURL, usersURL]) {
							const denied = await anonymous.get(url);
							expect(denied.status(), `unauthenticated ${new URL(url).pathname}`).toBe(401);
							expect((await denied.json()).code).toBe('unauthenticated');
						}
					} finally {
						await anonymous.dispose();
					}
					const login = await page.request.post(new URL('api/v1/session', origin).href, {
						data: { username: 'owner', password },
						failOnStatusCode: false
					});
					expect(login.status()).toBe(503);
					expect(login.headers()['retry-after']).toBeTruthy();
					expect(errors).toEqual([]);
					expect(violations).toEqual([]);
				} finally {
					await context.close();
				}
			});
		}
	}
} else {
	test('session fixture mode is explicit', () => {
		expect(['states', 'two-installations', 'local-access', 'recovery']).toContain(mode);
	});
}
