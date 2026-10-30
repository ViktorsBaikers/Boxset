// The browser half of AC-037: confirmed tabs share one authorized stream per
// installation, a tab leaving its route keeps it, a restart resyncs it with the cursor
// the tab applied, a late tab receives the cached hello, every visible state stays the
// existing one, a delivered frame never unmounts the ready surface, and another
// installation's frames reach none of this installation's tabs.
import { appendFileSync, statSync } from 'node:fs';
import AxeBuilder from '@axe-core/playwright';
import { expect, request, test, type Page } from '@playwright/test';

declare const process: { env: Record<string, string | undefined> };

const password = 'correct horse battery staple';
const origins: Record<string, string | undefined> = {
	root: process.env.S5_DEV_URL,
	media: process.env.S5_PREVIEW_URL
};
const switched = process.env.BOXSET_EVENTS_SWITCH;
const secondOrigin = process.env.BOXSET_EVENTS_SECOND;
const captureDir = process.env.BOXSET_CAPTURE_DIR;
const ready = 'Boxset is ready.';
const loading = 'Loading Boxset…';
const required = 'Sign in to check this Boxset.';
// The axe result of the row that scanned, folded into the harness counters file.
let rowAxe = 0;
let rowScans = 0;

type Counters = {
	requests: number;
	ended: number;
	cookie: boolean;
	hello: boolean;
	cursors: string[];
	last_id: string;
	hello_id: string;
	resync: { watermark: number; reason: string } | null;
	resync_id: string;
};

async function counters(page: Page): Promise<Counters> {
	return page.evaluate(async () => (await fetch('/__stream-probe')).json());
}

async function control(page: Page, origin: string, action: string): Promise<void> {
	const response = await page.request.post(
		new URL(`/__stream-control?action=${action}`, origin).href
	);
	expect(response.status(), `control ${action}`).toBe(204);
}

// statusRequests counts this page's own status reads: the app's one protected
// projection, so a delivered frame is observable as the refetch it caused.
function statusRequests(page: Page) {
	const counter = { value: 0 };
	page.on('request', (candidate) => {
		if (
			candidate.method() === 'GET' &&
			new URL(candidate.url()).pathname.endsWith('/api/v1/system/status')
		)
			counter.value += 1;
	});
	return counter;
}

async function settle(): Promise<void> {
	await new Promise((resolve) => setTimeout(resolve, 750));
}

// quiesce waits until a page's own request counter stops moving, so a baseline is not
// taken while the sign-in hello refetch is still landing.
async function quiesce(counter: { value: number }): Promise<void> {
	let seen = -1;
	while (seen !== counter.value) {
		seen = counter.value;
		await settle();
	}
}

const textSelector = 'main h1, main h2, main [role="status"] span, main button, main a';

// twoFrames waits for the browser to paint the frame the previous DOM change produced.
async function twoFrames(page: Page): Promise<void> {
	await page.evaluate(() => {
		const { promise, resolve } = Promise.withResolvers<void>();
		requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
		return promise;
	});
}

// expandText rewrites the surface's own copy to the shell matrix's expansion: accented
// glyphs at 1.5 line height with 0.12em letter spacing and 0.16em word spacing, so a
// cell proves the page still fits rather than the default copy.
async function expandText(page: Page, expanded: boolean, original: string[]): Promise<void> {
	await page.evaluate(
		({ selector, expanded, original }) => {
			const accents: Record<string, string> = { a: 'á', e: 'ë', i: 'ï', o: 'ö', u: 'ü' };
			document.querySelectorAll<HTMLElement>(selector).forEach((element, index) => {
				if (!expanded) {
					element.textContent = original[index];
					element.style.removeProperty('line-height');
					element.style.removeProperty('letter-spacing');
					element.style.removeProperty('word-spacing');
					if (element.tagName === 'SPAN')
						element.parentElement!.style.removeProperty('margin-bottom');
					return;
				}
				const pseudo = original[index].replace(
					/[aeiou]/gi,
					(letter) => accents[letter.toLowerCase()]!
				);
				element.textContent = `[${pseudo} ${pseudo}]`;
				element.style.lineHeight = '1.5';
				element.style.letterSpacing = '0.12em';
				element.style.wordSpacing = '0.16em';
				if (element.tagName === 'SPAN') element.parentElement!.style.marginBottom = '2em';
			});
		},
		{ selector: textSelector, expanded, original }
	);
}

// observeShifts sums the layout shifts this document reports without user input. The
// matrix row resets the sum once a cell has painted and requires it to stay zero while
// the cell sits still.
type ShiftWindow = Window & { __eventsShifts: number };

async function observeShifts(page: Page): Promise<void> {
	await page.evaluate(() => {
		const target = window as unknown as ShiftWindow;
		target.__eventsShifts = 0;
		new PerformanceObserver((list) => {
			for (const entry of list.getEntries()) {
				const shift = entry as PerformanceEntry & { value: number; hadRecentInput: boolean };
				if (!shift.hadRecentInput) target.__eventsShifts += shift.value;
			}
		}).observe({ type: 'layout-shift' });
	});
}

// online is the row's connectivity stub: the container has no route, so a headless
// profile would report every tab offline. Connectivity itself is exercised by the
// shell's own offline rows.
async function online(page: Page): Promise<void> {
	await page.addInitScript(() => {
		Object.defineProperty(navigator, 'onLine', { configurable: true, get: () => true });
	});
}

// violations is this document's own policy report: every securitypolicyviolation it
// received since its last navigation.
async function violations(page: Page): Promise<string[]> {
	return page.evaluate(
		() => (window as unknown as { __eventsViolations: string[] }).__eventsViolations ?? []
	);
}

test.beforeEach(async ({ context }) => {
	rowAxe = 0;
	rowScans = 0;
	await context.addInitScript(() => {
		Object.defineProperty(navigator, 'onLine', { configurable: true, get: () => true });
		const target = window as unknown as { __eventsViolations: string[] };
		target.__eventsViolations = [];
		document.addEventListener('securitypolicyviolation', (event) => {
			target.__eventsViolations.push(`${event.effectiveDirective} ${event.blockedURI}`);
		});
	});
});

// Every document a row leaves open is graded against the production policy, and the
// measured counts reach the run's PASS line through the harness file rather than a
// literal beside them.
test.afterEach(async ({ context }) => {
	let csp = 0;
	for (const page of context.pages()) {
		const found = await violations(page);
		expect(found, 'a document reported a policy violation').toEqual([]);
		csp += found.length;
	}
	const destination = process.env.BOXSET_EVENTS_COUNTERS;
	if (destination) appendFileSync(destination, `csp=${csp} axe=${rowAxe} scans=${rowScans}\n`);
});

// signInHere signs in from the sign-in link the current page already offers, the way a
// tab that lost its authority does: the leave and the returning confirm travel over the
// same tab, not a fresh document.
async function signInHere(page: Page, username = 'owner'): Promise<void> {
	await page.getByRole('link', { name: 'Sign in', exact: true }).click();
	await page.getByLabel('Username').fill(username);
	await page.getByLabel('Password', { exact: true }).fill(password);
	await page.getByRole('button', { name: 'Sign in', exact: true }).click();
	await expect(page.getByRole('status')).toHaveText(ready);
}

async function signIn(page: Page, origin: string, username = 'owner'): Promise<void> {
	await online(page);
	await page.goto(origin);
	await expect(page.getByRole('status')).toHaveText(
		/^\s*(Boxset is ready\.|Sign in to check this Boxset\.)\s*$/
	);
	const link = page.getByRole('link', { name: 'Sign in', exact: true });
	if (await link.isVisible()) await signInHere(page, username);
	await expect(page.getByRole('status')).toHaveText(ready);
}

// edit acts as the installation's own administrator over its HTTP API, so every frame
// the rows observe is published by a committed edit. A row whose usual administrator
// has been downgraded by an earlier edit names the actor that still holds the rights.
async function edit(
	origin: string,
	username: string,
	patch: { role?: string; disabled?: boolean },
	actor = 'editor'
): Promise<void> {
	// The edit is the installation's own administrator's: the row's tab may be the very
	// credential it changes.
	const api = await request.newContext();
	try {
		const login = await api.post(new URL('api/v1/session', origin).href, {
			data: { username: actor, password }
		});
		expect(login.status(), `${actor} signs in`).toBe(201);
		const listing = await api.get(new URL('api/v1/users', origin).href);
		expect(listing.status()).toBe(200);
		const page = (await listing.json()) as {
			items: Array<{ id: number; username: string; revision: number }>;
		};
		const target = page.items.find((item) => item.username === username);
		expect(target, `${username} is seeded`).toBeTruthy();
		const patched = await api.patch(new URL(`api/v1/users/${target!.id}`, origin).href, {
			data: { ...patch, expectedRevision: target!.revision }
		});
		expect(patched.status(), `edit ${username}`).toBe(200);
	} finally {
		await api.dispose();
	}
}

for (const [base, origin] of Object.entries(origins)) {
	if (!origin) throw new Error('event installation missing');

	test(`${base}: three confirmed tabs read one stream and the owner tab leaving keeps it`, async ({
		context
	}) => {
		const first = await context.newPage();
		await signIn(first, origin);
		const second = await context.newPage();
		await signIn(second, origin);
		const third = await context.newPage();
		await signIn(third, origin);

		const opened = await counters(third);
		expect(opened.cookie, 'the stream carried the session cookie').toBe(true);
		expect(opened.hello, 'hello arrived before the stream ended').toBe(true);
		expect(opened.ended).toBe(0);

		// The document that signed in and created the worker is graded before it leaves.
		expect(await violations(first), 'the owner document reported a policy violation').toEqual([]);
		await first.close();
		await expect(second.getByRole('status')).toHaveText(ready);
		// The surviving tabs keep one open stream between them; a re-established one has
		// announced its hello and refetched before the frame is published.
		await new Promise((resolve) => setTimeout(resolve, 2500));
		const secondStatuses = statusRequests(second);
		const thirdStatuses = statusRequests(third);
		const secondBefore = secondStatuses.value;
		const thirdBefore = thirdStatuses.value;
		// One committed edit publishes one users frame to both surviving tabs.
		await edit(origin, 'second', { role: 'member' });
		await expect.poll(() => thirdStatuses.value, { timeout: 10_000 }).toBe(thirdBefore + 1);
		await expect.poll(() => secondStatuses.value, { timeout: 10_000 }).toBe(secondBefore + 1);
		await settle();
		expect(thirdStatuses.value).toBe(thirdBefore + 1);
		expect(secondStatuses.value).toBe(secondBefore + 1);
		const after = await counters(third);
		expect(after.requests - after.ended, 'the owner exit left one open stream').toBe(1);
		expect(after.hello).toBe(true);
		await expect(third.getByRole('status')).toHaveText(ready);
		console.info(`EVENTS-SHARED base=${base} tabs=3 streams=1 owner_exit=kept frames=1`);
	});

	test(`${base}: a restart resyncs the shared stream, carries the cursor and refetches exactly once`, async ({
		page
	}) => {
		await signIn(page, origin);
		const statuses = statusRequests(page);
		// The sign-in hello refetch can land after this counter attaches: settle it so the
		// restart's own refetch is measured as a delta.
		await quiesce(statuses);
		const refetches = statuses.value;
		const before = await counters(page);
		const cursorCount = before.cursors.length;
		await control(page, origin, 'restart');
		// One re-established stream: the restart ended the old one and the tab reads the
		// new composition through exactly one open connection.
		await expect
			.poll(
				async () => {
					const state = await counters(page);
					return state.requests - state.ended;
				},
				{ timeout: 20_000 }
			)
			.toBe(1);
		await expect(page.getByRole('status')).toHaveText(ready);
		// The reconnect's delivered frame is a resync with its watermark: the server
		// closes that stream, so the worker opens one more from the recovery watermark.
		await expect
			.poll(async () => (await counters(page)).resync, { timeout: 15_000 })
			.toMatchObject({ reason: 'epoch' });
		const after = await counters(page);
		expect(
			after.requests - before.requests,
			'the restart did not take exactly the resync and live connections'
		).toBe(2);
		// The restarted composition published one frame before this reconnect, so the
		// resync watermark carries a real sequence and its id names it.
		const watermark = after.resync?.watermark ?? -1;
		expect(watermark, 'the resync watermark was a vacuous zero').toBeGreaterThan(0);
		expect(
			Number(after.hello_id.split('.').at(-1)),
			'the reconnected hello replays the recovery watermark'
		).toBe(watermark);
		expect(Number(after.resync_id.split('.').at(-1)), 'the resync id carries its watermark').toBe(
			watermark
		);
		expect(
			after.cursors.slice(cursorCount),
			'the reconnect names the applied frame, then the resync cursor'
		).toEqual([before.last_id, after.resync_id]);
		// Exactly one refetch: the resync refreshes the projection, and the reconnected
		// stream's duplicate hello of the same identity refreshes nothing.
		await expect.poll(() => statuses.value, { timeout: 15_000 }).toBe(refetches + 1);
		await settle();
		expect(statuses.value, 'the restart refetched more than once').toBe(refetches + 1);
		expect(await page.getByText('owner', { exact: true }).isVisible()).toBe(true);
		const scan = await new AxeBuilder({ page })
			.withTags(['wcag2a', 'wcag2aa', 'wcag21aa', 'wcag22aa'])
			.analyze();
		expect(scan.violations).toEqual([]);
		rowAxe += scan.violations.length;
		rowScans += 1;
		console.info(
			`EVENTS-RESTART base=${base} streams=${after.requests - before.requests} refetch=${statuses.value - refetches} resync=${after.resync?.reason} watermark=${after.resync?.watermark} cursor=${after.cursors.at(-1)} axe=${scan.violations.length}`
		);
	});

	test(`${base}: a tab that confirms after hello replays it and refetches exactly once`, async ({
		context
	}) => {
		const first = await context.newPage();
		await signIn(first, origin);
		const opened = await counters(first);
		// The worker must already hold the hello the late tab replays: opening it first
		// leaves the confirmation racing an uncached hello.
		await expect.poll(async () => (await counters(first)).hello, { timeout: 10_000 }).toBe(true);

		const late = await context.newPage();
		const statuses = statusRequests(late);
		let held = 0;
		let release!: () => void;
		const gate = new Promise<void>((resolve) => {
			release = resolve;
		});
		await late.route('**/api/v1/session', async (route) => {
			held += 1;
			if (held > 1) return route.continue();
			await gate;
			await route.continue();
		});
		await late.goto(origin);
		// The tab cannot confirm while its own session response is held, so the hello the
		// worker already cached arrived before the confirmation.
		await expect.poll(() => held, { timeout: 10_000 }).toBe(1);
		expect(statuses.value).toBe(0);
		release();
		await expect(late.getByRole('status')).toHaveText(ready);
		await expect.poll(() => statuses.value, { timeout: 10_000 }).toBe(2);
		await settle();
		expect(statuses.value, 'exactly one extra refetch from the replayed hello').toBe(2);
		expect((await counters(late)).requests).toBe(opened.requests);
		console.info(
			`EVENTS-LATE-CONFIRM base=${base} extra_refetch=${statuses.value - 1} streams=${opened.requests}`
		);
	});

	test(`${base}: signing out ends the shared stream into sign-in in every tab`, async ({
		context
	}) => {
		// Either the worker's own loss signal or the stream's next revalidation ends the
		// other tab's stream; both are the installation's current-authority decision.
		test.setTimeout(90_000);
		const first = await context.newPage();
		await signIn(first, origin);
		const second = await context.newPage();
		await signIn(second, origin);
		const before = await counters(second);
		await first.getByRole('button', { name: 'Sign out' }).click();
		await expect(first.getByRole('heading', { name: 'Sign in', exact: true })).toBeVisible();
		await expect(second.getByRole('status'), 'the other tab kept stale authority').toHaveText(
			required,
			{ timeout: 30_000 }
		);
		expect(await second.getByText('owner', { exact: true }).count()).toBe(0);
		await expect.poll(async () => (await counters(second)).ended).toBeGreaterThan(before.ended);
		console.info(`EVENTS-SIGN-OUT base=${base} tabs=2 state=sign-in`);
	});

	test(`${base}: disabling a tab's user ends its stream into sign-in`, async ({ context }) => {
		const page = await context.newPage();
		await signIn(page, origin, 'disabled');
		const before = await counters(page);
		await edit(origin, 'disabled', { disabled: true });
		await expect(page.getByRole('status'), 'a disabled credential stays private').toHaveText(
			required,
			{ timeout: 15_000 }
		);
		expect(await page.getByText('disabled', { exact: true }).count()).toBe(0);
		await expect.poll(async () => (await counters(page)).ended).toBeGreaterThan(before.ended);
		console.info(`EVENTS-DISABLED base=${base} state=sign-in`);
	});

	test(`${base}: an expired session ends in sign-in and the same identity signs in again on one new stream`, async ({
		context
	}) => {
		test.setTimeout(90_000);
		const page = await context.newPage();
		await page.goto(origin);
		const before = await counters(page);
		await signIn(page, origin, 'expired');
		const confirmed = await counters(page);
		expect(confirmed.requests - before.requests, 'the sign-in opened no stream').toBe(1);
		await control(page, origin, 'expire');
		await expect(page.getByRole('status'), 'an expired session stays private').toHaveText(
			required,
			{ timeout: 30_000 }
		);
		// The refusal is a generation, not an identity: the same user signing in again
		// opens exactly one more stream instead of staying refused for every tab.
		const refused = await counters(page);
		expect(
			refused.requests - confirmed.requests,
			'the refused reconnect is the recorded generation'
		).toBe(1);
		await signInHere(page, 'expired');
		const reopened = await counters(page);
		expect(reopened.requests, 'the same triple never reconnected').toBe(refused.requests + 1);
		expect(reopened.requests - reopened.ended).toBe(1);

		// A tab opened after the refusal joins the live stream from the cached hello: it
		// opens no new fetch, and the next frame reaches it.
		const late = await context.newPage();
		const lateStatuses = statusRequests(late);
		await late.goto(origin);
		await expect(late.getByRole('status')).toHaveText(ready);
		expect((await counters(late)).requests).toBe(reopened.requests);
		// access() reads status on mount, so the baseline, not zero, is the proof: the
		// delivered frame must add exactly one refetch.
		await quiesce(lateStatuses);
		const lateRefetches = lateStatuses.value;
		await edit(origin, 'expired', { role: 'admin' });
		await expect.poll(() => lateStatuses.value, { timeout: 10_000 }).toBe(lateRefetches + 1);
		await settle();
		expect(lateStatuses.value, 'the fresh tab refetched more than once').toBe(lateRefetches + 1);
		console.info(
			`EVENTS-EXPIRED base=${base} state=sign-in refused_get=1 reopened_get=1 fresh_tab_fetch=${lateStatuses.value - lateRefetches}`
		);
	});

	test(`${base}: a downgrade that keeps status:read keeps the ready page and only replaces the stream`, async ({
		context
	}) => {
		const page = await context.newPage();
		await signIn(page, origin, 'downgradee');
		const before = await counters(page);
		await edit(origin, 'downgradee', { role: 'viewer' });
		// The affected stream closed and one was re-established; the page stays ready.
		await expect
			.poll(async () => (await counters(page)).requests, { timeout: 15_000 })
			.toBe(before.requests + 1);
		await expect(page.getByRole('status')).toHaveText(ready);
		expect(await page.getByText('downgradee', { exact: true }).isVisible()).toBe(true);
		expect(await page.getByRole('heading', { name: 'Sign in', exact: true }).count()).toBe(0);
		expect(await page.getByText('You do not have permission').count()).toBe(0);
		console.info(`EVENTS-DOWNGRADE base=${base} state=ready streams=${before.requests + 1}`);
	});

	test(`${base}: an unconfirmed tab opens no stream and shows no protected state`, async ({
		page
	}) => {
		await online(page);
		await page.goto(origin);
		await expect(page.getByRole('status')).toHaveText(required);
		const before = await counters(page);
		await settle();
		const after = await counters(page);
		expect(after.requests, 'an unconfirmed tab opened a stream').toBe(before.requests);
		expect(await page.getByText('owner', { exact: true }).count()).toBe(0);
		console.info(`EVENTS-UNCONFIRMED base=${base} streams=0 state=sign-in`);
	});

	test(`${base}: a tab that leaves its route keeps the other tabs' stream`, async ({ context }) => {
		const first = await context.newPage();
		await signIn(first, origin);
		const second = await context.newPage();
		await signIn(second, origin);
		const before = await counters(second);
		const statuses = statusRequests(second);
		// The sign-in hello refetch can land after this counter attaches: settle it so the
		// delivered frame is measured as a delta.
		await quiesce(statuses);
		const delivered = statuses.value;

		// Client-side navigation to the sign-in route: this tab leaves, it does not end the
		// installation's session.
		const target = new URL('sign-in', origin).pathname;
		await first.evaluate((path) => {
			const link = document.createElement('a');
			link.href = path;
			link.textContent = 'leave';
			document.body.append(link);
			link.click();
		}, target);
		await first.waitForURL((url) => url.pathname === target, { timeout: 10_000 });
		await settle();

		// The surviving tab keeps one open stream and still receives the next frame.
		await edit(origin, 'second', { role: 'member' });
		await expect.poll(() => statuses.value, { timeout: 10_000 }).toBe(delivered + 1);
		await settle();
		expect(statuses.value, 'the delivered frame refetched more than once').toBe(delivered + 1);
		const after = await counters(second);
		expect(after.requests, 'a leaving tab opened another stream').toBe(before.requests);
		expect(after.requests - after.ended, 'a leaving tab ended the shared stream').toBe(1);
		await expect(second.getByRole('status')).toHaveText(ready);

		// Closing the last confirmed document tells the worker to leave: with no port left
		// it ends the installation's stream instead of letting it outlive every tab.
		await second.evaluate(() => window.dispatchEvent(new PageTransitionEvent('pagehide')));
		await expect
			.poll(async () => (await counters(first)).ended, { timeout: 10_000 })
			.toBe(after.ended + 1);
		const closed = await counters(first);
		expect(closed.requests - closed.ended, 'an authorized stream outlived its last tab').toBe(0);
		console.info(
			`EVENTS-LEAVE base=${base} state=sign-in streams=${after.requests} ended=${after.ended} frames=1 closed=${closed.requests - closed.ended}`
		);
	});

	test(`${base}: a tab whose own sign-out attempt fails keeps the other tabs' stream`, async ({
		context
	}) => {
		const first = await context.newPage();
		await signIn(first, origin);
		const second = await context.newPage();
		await signIn(second, origin);
		const before = await counters(second);
		const statuses = statusRequests(second);
		await first.route('**/api/v1/session', async (route) => {
			if (route.request().method() !== 'DELETE') return route.continue();
			return route.fulfill({
				status: 500,
				contentType: 'application/problem+json',
				body: JSON.stringify({ code: 'internal' })
			});
		});
		await first.getByRole('button', { name: 'Sign out' }).click();
		await expect(
			first.getByText('Sign-out could not be confirmed. Retry to end this session.')
		).toBeVisible();
		// A failed attempt is a leave, not a settled loss: the other tab keeps its stream
		// and the next frame arrives.
		await edit(origin, 'second', { role: 'member' });
		await expect.poll(() => statuses.value, { timeout: 10_000 }).toBe(1);
		const after = await counters(second);
		expect(after.requests).toBe(before.requests);
		expect(after.requests - after.ended).toBe(1);
		await expect(second.getByRole('status')).toHaveText(ready);
		console.info(`EVENTS-SIGN-OUT-FAILED base=${base} state=sign-out-error streams=1 frames=1`);
	});

	test(`${base}: a system frame keeps the ready surface painted and a job frame refetches nothing`, async ({
		page
	}) => {
		await signIn(page, origin);
		const statuses = statusRequests(page);
		await page.getByRole('button', { name: 'Sign out' }).focus();
		// The sign-in hello refetch can still be in flight: settle it before the frame so
		// the measurement is the frame's own single refetch.
		await quiesce(statuses);
		const refetches = statuses.value;
		let held = 0;
		let release!: () => void;
		const gate = new Promise<void>((resolve) => {
			release = resolve;
		});
		await page.route('**/api/v1/system/status', async (route) => {
			held += 1;
			await gate;
			await route.continue();
		});
		await settle();
		expect(held, 'the baseline still had a status read in flight').toBe(0);
		await control(page, origin, 'system');
		await expect.poll(() => held, { timeout: 10_000 }).toBe(1);
		// The painted state, its focused control and the status node survive the frame's
		// refetch: mark the node so a remount is visible.
		await page.evaluate(() => {
			document.querySelector('[role="status"]')?.setAttribute('data-surface', 'kept');
		});
		await expect(page.getByRole('status')).toHaveText(ready);
		expect(await page.getByText(loading).count(), 'the loading branch replaced the ready one').toBe(
			0
		);
		expect(await page.evaluate(() => document.activeElement?.textContent)).toBe('Sign out');
		expect(statuses.value, 'one system frame issued more than one refetch').toBe(refetches + 1);
		release();
		await expect(page.getByRole('status')).toHaveText(ready);
		expect(
			await page.locator('[role="status"][data-surface="kept"]').count(),
			'the status node was replaced across the release'
		).toBe(1);
		expect(await page.evaluate(() => document.activeElement?.textContent)).toBe('Sign out');
		const delivered = statuses.value;
		// A users frame publishes the projection this route reads, so the control proves
		// this tab is receiving frames before the job half asserts silence.
		await edit(origin, 'second', { role: 'admin' });
		await expect.poll(() => statuses.value, { timeout: 10_000 }).toBe(delivered + 1);
		await settle();
		expect(statuses.value, 'the users control issued more than one refetch').toBe(delivered + 1);
		const afterUsers = statuses.value;
		// A job frame names a collection no route subscribes to, so it refetches nothing.
		await control(page, origin, 'job');
		await settle();
		expect(statuses.value, 'a job frame refetched the access projection').toBe(afterUsers);
		console.info(
			`EVENTS-READY-PAINT base=${base} system_refetch=${delivered - refetches} users_refetch=${afterUsers - delivered} job_refetch=${statuses.value - afterUsers} focus=kept`
		);
	});

	test(`${base}: the ready and sign-in states sweep the shell matrix with a keyboard focus ring`, async ({
		context
	}) => {
		test.setTimeout(240_000);
		expect(captureDir, 'BOXSET_CAPTURE_DIR').toBeTruthy();
		const page = await context.newPage();
		await page.setViewportSize({ width: 320, height: 720 });
		await page.emulateMedia({ colorScheme: 'light', reducedMotion: 'no-preference' });
		await page.goto(origin);
		await page.evaluate(() => {
			document.documentElement.dataset.theme = 'light';
		});
		await expect(page.getByRole('status')).toHaveText(required);
		const capture = async (state: string) => {
			expect(await page.evaluate(() => window.innerWidth)).toBe(320);
			expect(await page.evaluate(() => document.documentElement.dataset.theme)).toBe('light');
			const path = `${captureDir}/events-${base}-${state}-320-light.png`;
			await page.screenshot({ path, fullPage: false, animations: 'disabled', scale: 'css' });
			expect(statSync(path).size, `${state} capture is empty`).toBeGreaterThan(20);
		};

		// sweep measures every cell of the shell matrix for one visible state: page fit at
		// the four widths, both themes and motion settings, with and without expanded text;
		// a real keyboard focus ring on the first tab stop; and no layout shift while the
		// cell sits still. Each measured cell is a line in the harness counters file.
		const countersPath = process.env.BOXSET_EVENTS_COUNTERS;
		const sweep = async (state: string): Promise<number> => {
			// The webfont decides the copy's metrics; a cell measured mid-swap would report
			// the fallback face's overflow instead of the page's.
			await page.evaluate(async () => {
				await document.fonts.ready;
			});
			await observeShifts(page);
			const original = await page.locator(textSelector).allTextContents();
			let measured = 0;
			for (const width of [320, 768, 1024, 1440]) {
				await page.setViewportSize({ width, height: 720 });
				for (const theme of ['light', 'dark'] as const) {
					for (const motion of ['reduce', 'no-preference'] as const) {
						await page.emulateMedia({ colorScheme: theme, reducedMotion: motion });
						await page.evaluate((value) => {
							document.documentElement.dataset.theme = value;
						}, theme);
						for (const expanded of [false, true]) {
							await expandText(page, expanded, original);
							await twoFrames(page);
							const cell = `${base} ${state} ${width} ${theme} ${motion} text=${expanded ? 'expanded' : 'base'}`;
							// Links stay in the document-wide width check only: an inline
							// element's client box is zero, so comparing its own scroll width
							// would fail on any copy.
							const unfit = await page.locator('main').evaluate((main) => {
								const problems: string[] = [];
								const bounds = main.getBoundingClientRect();
								if (document.documentElement.scrollWidth > innerWidth)
									problems.push(`document ${document.documentElement.scrollWidth} > ${innerWidth}`);
								if (bounds.left < 0 || bounds.right > innerWidth)
									problems.push(`main ${bounds.left}..${bounds.right}`);
								for (const element of main.querySelectorAll<HTMLElement>('h1,h2,p,button')) {
									// Widths only: client and scroll heights round in opposite
									// directions, so a text line would fail this by one pixel
									// while the page scrolls vertically by design.
									if (element.scrollWidth > element.clientWidth)
										problems.push(
											`${element.tagName} ${element.scrollWidth}/${element.clientWidth}`
										);
								}
								return problems;
							});
							expect(unfit, `page did not fit at ${cell}`).toEqual([]);
							// Each ring is reached by a real Tab press. The body focus below
							// only resets the sequential start point; the assertion itself
							// requires :focus-visible and a non-zero outline on the element
							// the key landed on.
							await page.evaluate(() => {
								document.body.tabIndex = -1;
								document.body.focus();
								document.body.removeAttribute('tabindex');
							});
							let ring = 'none';
							for (let press = 0; press < 2 && ring === 'none'; press += 1) {
								await page.keyboard.press('Tab');
								ring = await page.evaluate(() => {
									const active = document.activeElement as HTMLElement | null;
									if (!active || active === document.body) return 'none';
									const style = getComputedStyle(active);
									return active.matches(':focus-visible') &&
										style.outlineStyle !== 'none' &&
										Number.parseFloat(style.outlineWidth) > 0
										? `${active.tagName.toLowerCase()}:${style.outlineWidth}`
										: 'none';
								});
							}
							expect(ring, `no keyboard focus ring at ${cell}`).not.toBe('none');
							await twoFrames(page);
							await page.evaluate(() => {
								const target = window as unknown as ShiftWindow;
								target.__eventsShifts = 0;
							});
							await twoFrames(page);
							const shifted = await page.evaluate(() => {
								const target = window as unknown as ShiftWindow;
								return target.__eventsShifts;
							});
							expect(shifted, `layout shift at ${cell}`).toBe(0);
							if (countersPath)
								appendFileSync(
									countersPath,
									`cell base=${base} state=${state} width=${width} theme=${theme} motion=${motion} text=${expanded ? 'expanded' : 'base'}\n`
								);
							measured += 1;
						}
					}
				}
			}
			await expandText(page, false, original);
			await page.setViewportSize({ width: 320, height: 720 });
			await page.emulateMedia({ colorScheme: 'light', reducedMotion: 'no-preference' });
			await page.evaluate(() => {
				document.documentElement.dataset.theme = 'light';
			});
			await twoFrames(page);
			return measured;
		};

		await capture('sign-in');
		const signInCells = await sweep('sign-in');
		await signInHere(page);
		await expect(page.getByText('owner', { exact: true })).toBeVisible();
		await settle();
		await capture('ready');
		const readyCells = await sweep('ready');
		console.info(
			`EVENTS-MATRIX base=${base} states=sign-in,ready cells=${signInCells + readyCells}`
		);
	});
}

test('two installations with equal user id and epoch stay isolated by installation', async ({
	context
}) => {
	test.setTimeout(90_000);
	if (!switched) throw new Error('the switching origin is missing');
	if (!secondOrigin) throw new Error('the second installation is missing');

	const first = await context.newPage();
	await signIn(first, switched);
	expect(await first.getByText('owner', { exact: true }).isVisible()).toBe(true);
	const opened = await counters(first);

	// The premise is asserted, not instructed: both installations' own session reads must
	// agree on user id and restore epoch and differ only in installId.
	const api = await request.newContext();
	let secondSession!: { installId: string; restoreEpoch: number; user: { id: number } };
	try {
		const login = await api.post(new URL('api/v1/session', secondOrigin).href, {
			data: { username: 'installed-b', password }
		});
		expect(login.status(), 'the second installation signs in').toBe(201);
		const read = await api.get(new URL('api/v1/session', secondOrigin).href);
		expect(read.status()).toBe(200);
		secondSession = (await read.json()) as typeof secondSession;
		await context.addCookies((await api.storageState()).cookies);
	} finally {
		await api.dispose();
	}
	const firstRead = await context.request.get(new URL('api/v1/session', switched).href);
	expect(firstRead.status()).toBe(200);
	const firstSession = (await firstRead.json()) as typeof secondSession;
	expect(firstSession.user.id, 'the two installations agree on user id').toBe(
		secondSession.user.id
	);
	expect(firstSession.restoreEpoch).toBe(secondSession.restoreEpoch);
	expect(firstSession.installId, 'the two installations are distinct').not.toBe(
		secondSession.installId
	);

	// Hold this tab's own session refetch so it stays confirmed on the first installation
	// while the origin already names the second one.
	let held = 0;
	let release!: () => void;
	const gate = new Promise<void>((resolve) => {
		release = resolve;
	});
	await first.route('**/api/v1/session', async (route) => {
		held += 1;
		const response = await route.fetch();
		await gate;
		await route.fulfill({ response });
	});
	await control(first, switched, 'switch');
	await expect.poll(() => held, { timeout: 15_000 }).toBe(1);
	await settle();
	expect(held, 'the identity change issued more than one refetch').toBe(1);

	// Positive control: a tab confirmed on the second installation receives its frame, so
	// the first tab's silence is isolation rather than a publisher that reached nobody.
	const witness = await context.newPage();
	const witnessed = statusRequests(witness);
	await signIn(witness, secondOrigin, 'installed-b');
	await expect(witness.getByText('installed-b', { exact: true })).toBeVisible();
	await quiesce(witnessed);
	const witnessedBefore = witnessed.value;

	// A committed edit in the second installation publishes a users frame. The tab is
	// still confirmed on the first one, so it must receive none: a relayed frame would
	// restart its held session read. The edited user is not the witness, because the same
	// commit closes the edited principal's streams before their queued frame is written.
	await edit(secondOrigin, 'editor', { role: 'member' });
	await expect.poll(() => witnessed.value, { timeout: 10_000 }).toBe(witnessedBefore + 1);
	await settle();
	expect(witnessed.value, 'the witness refetched more than once').toBe(witnessedBefore + 1);
	expect(held, 'a second installation frame reached a tab confirmed on the first').toBe(1);
	// The witness's own stream would otherwise count in the second installation's probe.
	expect(await violations(witness), 'the witness reported a policy violation').toEqual([]);
	await witness.close();

	// Only after the observation is the held response released: the tab reconfirms with
	// the valid second triple and receives that installation's frames.
	release();
	await expect(first.getByText('owner', { exact: true })).toHaveCount(0);
	await expect(first.getByText('installed-b', { exact: true })).toBeVisible({ timeout: 15_000 });
	await expect(first.getByRole('status')).toHaveText(ready);
	await expect
		.poll(
			async () => {
				const state = await counters(first);
				return state.requests - state.ended;
			},
			{ timeout: 15_000 }
		)
		.toBe(1);
	// One more committed edit in the second installation publishes to this tab: the
	// reconfirmation, not merely a shared open stream, is what receives B's frames.
	const reconfirmed = statusRequests(first);
	await quiesce(reconfirmed);
	const reconfirmedBefore = reconfirmed.value;
	await edit(secondOrigin, 'editor', { role: 'admin' }, 'installed-b');
	await expect.poll(() => reconfirmed.value, { timeout: 10_000 }).toBe(reconfirmedBefore + 1);
	await settle();
	expect(reconfirmed.value, 'the reconfirmed tab refetched more than once').toBe(
		reconfirmedBefore + 1
	);
	console.info(
		`EVENTS-ISOLATION installations=2 userId=${firstSession.user.id} epoch=${firstSession.restoreEpoch} distinct_install=true frames_crossed=${held - 1} witness_refetch=${witnessed.value - witnessedBefore} b_refetch=${reconfirmed.value - reconfirmedBefore} streams=${opened.requests}`
	);
});
