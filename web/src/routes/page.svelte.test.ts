import { afterAll, afterEach, expect, test, vi } from 'vitest';
import { page, userEvent } from 'vitest/browser';
import { cleanup, render } from 'vitest-browser-svelte';
import type { Snippet } from 'svelte';
import Layout from './+layout.svelte';
import Page from './+page.svelte';
import { goto } from '$app/navigation';
import { status } from '../lib/api';
import { onlineManager } from '@tanstack/svelte-query';
import { access } from '../lib/session';
import type { Session } from '../lib/api';

vi.mock('$app/navigation', () => ({ goto: vi.fn() }));

const http = vi.hoisted(() => {
	const original = globalThis.fetch.bind(globalThis);
	const fixture: { handle?: typeof fetch } = {};
	globalThis.fetch = (...args) => {
		const input = args[0];
		const path = new URL(input instanceof Request ? input.url : String(input), location.href)
			.pathname;
		if (fixture.handle && (path.endsWith('/ping') || path.includes('/api/')))
			return fixture.handle(...args);
		return original(...args);
	};
	return {
		fixture,
		restore: () => {
			globalThis.fetch = original;
		}
	};
});

afterAll(() => http.restore());

const pending: Array<() => void> = [];
const calls: string[] = [];

test('access adopts current role revision and rights after fresh confirmation', async () => {
	async function result(work: ReturnType<typeof access>) {
		let timer: ReturnType<typeof setTimeout>;
		try {
			return await Promise.race([
				work,
				new Promise<never>((_, reject) => {
					timer = setTimeout(() => reject(new Error('adoption result deadline')), 5000);
				})
			]);
		} finally {
			clearTimeout(timer!);
		}
	}
	const before: Session = {
		user: {
			id: 1,
			username: 'private-owner',
			role: 'admin',
			revision: 1,
			disabled: false,
			createdAt: '2026-10-24T00:00:00Z'
		},
		rights: ['status:read', 'users:read', 'users:write'],
		credential: 'cookie',
		restoreEpoch: 1,
		expiresAt: '2026-11-23T00:00:00Z',
		localBypassEnabled: false,
		canLogout: true
	};
	const after: Session = {
		user: {
			id: 1,
			username: 'private-owner',
			role: 'viewer',
			revision: 2,
			disabled: false,
			createdAt: '2026-10-24T00:00:00Z'
		},
		rights: ['status:read'],
		credential: 'cookie',
		restoreEpoch: 1,
		expiresAt: '2026-11-23T00:00:00Z',
		localBypassEnabled: false,
		canLogout: true
	};
	const snapshot = {
		state: 'ready',
		version: 'test',
		schemaVersion: 4,
		restoreEpoch: 1,
		startedAt: '2026-10-24T00:00:00Z',
		timings: { migrationMs: 0, recoveryMs: 0 },
		goMemLimitBytes: 1048576
	};
	let current = before;
	let requests = 0;
	let delivered = 0;
	let release!: () => void;
	let holdTimer: ReturnType<typeof setTimeout>;
	const held = new Promise<void>((resolve, reject) => {
		release = () => {
			clearTimeout(holdTimer);
			resolve();
		};
		holdTimer = setTimeout(() => reject(new Error('adoption session hold deadline')), 5000);
	});
	void held.catch(() => {});
	pending.push(release);
	http.fixture.handle = async (input, init) => {
		const request =
			input instanceof Request ? input : new Request(new URL(String(input), location.href), init);
		expect(request.method).toBe('GET');
		expect(request.cache).toBe('no-store');
		const path = new URL(request.url).pathname;
		if (path === '/ping')
			return new Response('ok', {
				headers: { 'X-Boxset-State': 'ready', 'Cache-Control': 'no-store' }
			});
		if (path === '/api/v1/system/status') return Response.json(snapshot);
		expect(request.url).toBe(new URL('/api/v1/session', location.href).href);
		requests++;
		if (requests === 2) await held;
		delivered++;
		return Response.json(current, { headers: { 'Cache-Control': 'no-store' } });
	};
	try {
		const first = await result(access());
		expect(first).toEqual({ state: 'ready', session: before, status: snapshot });
		expect(requests).toBe(1);
		current = after;
		let settled = false;
		const second = access().finally(() => {
			settled = true;
		});
		void second.catch(() => {});
		await expect.poll(() => requests, { timeout: 5000 }).toBe(2);
		expect(delivered).toBe(1);
		expect(settled).toBe(false);
		release();
		const adopted = await result(second);
		expect(delivered).toBe(2);
		console.info('ACCESS-ADOPTION fetched=2 revision=2 restricted=true');
		expect(adopted.state).toBe('ready');
		expect(adopted.status).toEqual(snapshot);
		expect(
			adopted.session,
			'ADOPTION returned current authorization after fetched session'
		).toEqual(after);
		const adoptedUser = adopted.session?.user;
		if (!adoptedUser) throw new Error('adoption lost the confirmed user');
		expect(adoptedUser.id).toBe(1);
		expect(adoptedUser.role).toBe('viewer');
		expect(adoptedUser.revision).toBe(2);
		expect(adopted.session?.rights).toEqual(['status:read']);
		expect(adopted.session?.rights).not.toContain('users:read');
		expect(adopted.session?.rights).not.toContain('users:write');
		console.info(`ACCESS-ADOPTION assertions=${expect.getState().assertionCalls}`);
	} finally {
		release();
		http.fixture.handle = undefined;
	}
});

function holdLifecycle(state = 'ready') {
	let release!: () => void;
	const held = new Promise<void>((resolve) => {
		release = resolve;
	});
	pending.push(release);
	http.fixture.handle = async (input, init) => {
		const path = new URL(input instanceof Request ? input.url : String(input), location.href)
			.pathname;
		calls.push(path);
		if (path.endsWith('/ping')) {
			await held;
			const signal = input instanceof Request ? input.signal : init?.signal;
			signal?.throwIfAborted();
			return new Response('ok', {
				headers: { 'X-Boxset-State': state, 'Cache-Control': 'no-store' }
			});
		}
		if (path.endsWith('/api/v1/session'))
			return Response.json({ status: 401, code: 'unauthenticated' }, { status: 401 });
		throw new Error('unexpected protected request');
	};
	return { release };
}

async function renderPage() {
	await render(Layout, { props: { children: Page as unknown as Snippet } });
}

async function publicPending() {
	await expect.element(page.getByRole('status')).toHaveTextContent('Loading Boxset…');
	expect(document.querySelector('form, input, textarea, select, a')).toBeNull();
	expect(calls.every((path) => path === '/ping')).toBe(true);
}

async function required() {
	await expect.element(page.getByRole('status')).toHaveTextContent('Sign in to check this Boxset.');
	await expect
		.element(page.getByRole('link', { name: 'Sign in' }))
		.toHaveAttribute('href', '/sign-in');
	expect(calls).toEqual(['/ping', '/api/v1/session']);
	expect(document.querySelector('form, input, textarea, select')).toBeNull();
}

function setOnLine(value: boolean) {
	Object.defineProperty(navigator, 'onLine', { configurable: true, get: () => value });
}

afterEach(async () => {
	await cleanup();
	for (const release of pending.splice(0)) release();
	http.fixture.handle = undefined;
	calls.length = 0;
	// Removing the own property restores the browser's real getter.
	Reflect.deleteProperty(navigator, 'onLine');
	Reflect.deleteProperty(document, 'visibilityState');
	window.dispatchEvent(new Event('online'));
	onlineManager.setOnline(true);
	document.dispatchEvent(new Event('visibilitychange'));
	vi.mocked(goto).mockReset();
	vi.restoreAllMocks();
});

function setVisible(value: boolean) {
	Object.defineProperty(document, 'visibilityState', {
		configurable: true,
		get: () => (value ? 'visible' : 'hidden')
	});
	document.dispatchEvent(new Event('visibilitychange'));
}

function sessionHTTP() {
	let revoked = false;
	let endedCode = 'credential_invalid';
	let deleted = 0;
	let lifecycleState = 'ready';
	let deleteFailure: 'none' | 'transport' | 'lost' | 'server' | 'unexpected401' = 'none';
	let holdStatus = false;
	let holdConfirmation = false;
	const confirmations: Array<() => void> = [];
	const statusRequests: Array<{ signal?: AbortSignal | null; release: () => void }> = [];
	http.fixture.handle = async (input, init) => {
		const request =
			input instanceof Request ? input : new Request(new URL(String(input), location.href), init);
		const path = new URL(request.url).pathname;
		calls.push(path);
		if (request.method === 'DELETE') {
			deleted++;
			if (lifecycleState !== 'ready')
				return Response.json({ status: 401, code: 'unauthenticated' }, { status: 401 });
			if (deleteFailure === 'transport') throw new TypeError('synthetic transport failure');
			if (deleteFailure === 'server')
				return Response.json({ status: 503, code: 'draining' }, { status: 503 });
			if (deleteFailure === 'unexpected401')
				return Response.json({ status: 401, code: 'unexpected_response' }, { status: 401 });
			if (revoked) return Response.json({ status: 401, code: endedCode }, { status: 401 });
			revoked = true;
			if (deleteFailure === 'lost') throw new TypeError('synthetic lost response');
			return new Response(null, { status: 204 });
		}
		expect(request.cache).toBe('no-store');
		if (path === '/ping') {
			if (holdConfirmation)
				await new Promise<void>((release) => {
					confirmations.push(release);
					pending.push(release);
				});
			return new Response('ok', {
				headers: { 'X-Boxset-State': lifecycleState, 'Cache-Control': 'no-store' }
			});
		}
		if (lifecycleState !== 'ready')
			return Response.json({ status: 401, code: 'unauthenticated' }, { status: 401 });
		if (revoked) return Response.json({ status: 401, code: endedCode }, { status: 401 });
		if (path === '/api/v1/session')
			return Response.json({
				user: {
					id: 1,
					username: 'private-owner',
					role: 'admin',
					revision: 1,
					disabled: false,
					createdAt: '2026-10-24T00:00:00Z'
				},
				rights: ['status:read', 'users:read', 'users:write'],
				credential: 'cookie',
				restoreEpoch: 1,
				expiresAt: '2026-11-23T00:00:00Z',
				localBypassEnabled: false,
				canLogout: true
			});
		if (path === '/api/v1/system/status') {
			if (holdStatus)
				await new Promise<void>((release) => {
					statusRequests.push({ signal: request.signal, release });
					pending.push(release);
				});
			// A response already authorized before hiding may still arrive after cancellation.
			return Response.json({
				state: 'ready',
				version: 'test',
				schemaVersion: 4,
				restoreEpoch: 1,
				startedAt: '2026-10-24T00:00:00Z',
				timings: { migrationMs: 0, recoveryMs: 0 },
				goMemLimitBytes: 1048576
			});
		}
		throw new Error('unexpected request');
	};
	setOnLine(true);
	setVisible(true);
	return {
		statusRequests,
		state: (value: string) => {
			lifecycleState = value;
		},
		confirmations,
		revoke: (code = 'credential_invalid') => {
			revoked = true;
			endedCode = code;
		},
		hold: () => {
			holdStatus = true;
		},
		holdConfirmation: () => {
			holdConfirmation = true;
		},
		failDelete: (value: typeof deleteFailure) => {
			deleteFailure = value;
		},
		deletes: () => deleted
	};
}

test('hiding an initial held confirmation cancels it and resume cannot publish its old authority', async () => {
	const fixture = sessionHTTP();
	fixture.hold();
	await renderPage();
	await expect.poll(() => fixture.statusRequests.length).toBe(1);
	setVisible(false);
	await expect.poll(() => fixture.statusRequests[0].signal?.aborted).toBe(true);
	fixture.revoke();
	fixture.holdConfirmation();
	setVisible(true);
	await expect.poll(() => fixture.confirmations.length).toBe(1);
	fixture.statusRequests[0].release();
	await expect.element(page.getByRole('status')).toHaveTextContent('Loading Boxset…');
	await expect.element(page.getByText('private-owner', { exact: true })).not.toBeInTheDocument();
	fixture.confirmations[0]();
	await expect.element(page.getByRole('status')).toHaveTextContent('Sign in to check this Boxset.');
	await expect.element(page.getByText('private-owner', { exact: true })).not.toBeInTheDocument();
	expect(calls.filter((path) => path === '/api/v1/session')).toHaveLength(2);
});

for (const transition of ['visibility', 'offline'] as const) {
	test(`${transition} resume with retained identity stays public through fresh held confirmation and denial`, async () => {
		const fixture = sessionHTTP();
		await renderPage();
		await expect.element(page.getByText('private-owner', { exact: true })).toBeVisible();
		fixture.hold();
		if (transition === 'visibility') {
			setVisible(false);
			setVisible(true);
		} else {
			setOnLine(false);
			window.dispatchEvent(new Event('offline'));
			setOnLine(true);
			await page.getByRole('button', { name: 'Retry' }).click();
		}
		await expect.poll(() => fixture.statusRequests.length).toBe(1);
		await expect.element(page.getByRole('status')).toHaveTextContent('Loading Boxset…');
		await expect.element(page.getByText('private-owner', { exact: true })).not.toBeInTheDocument();
		if (transition === 'visibility') setVisible(false);
		else {
			setOnLine(false);
			window.dispatchEvent(new Event('offline'));
		}
		await expect.poll(() => fixture.statusRequests[0].signal?.aborted).toBe(true);
		fixture.revoke();
		fixture.holdConfirmation();
		if (transition === 'visibility') setVisible(true);
		else {
			setOnLine(true);
			window.dispatchEvent(new Event('online'));
		}
		await expect.poll(() => fixture.confirmations.length).toBe(1);
		fixture.statusRequests[0].release();
		await expect.element(page.getByRole('status')).toHaveTextContent('Loading Boxset…');
		await expect.element(page.getByText('private-owner', { exact: true })).not.toBeInTheDocument();
		fixture.confirmations[0]();
		await expect
			.element(page.getByRole('status'))
			.toHaveTextContent('Sign in to check this Boxset.');
		await expect.element(page.getByText('private-owner', { exact: true })).not.toBeInTheDocument();
	});
}

test('sign-out attempts transport and recovers while the global online manager is stale offline', async () => {
	const fixture = sessionHTTP();
	await renderPage();
	await expect.element(page.getByText('private-owner', { exact: true })).toBeVisible();
	onlineManager.setOnline(false);
	fixture.failDelete('transport');
	await page.getByRole('button', { name: 'Sign out' }).click();
	await expect.poll(() => fixture.deletes()).toBe(1);
	await expect
		.element(page.getByRole('status'))
		.toHaveTextContent('Sign-out could not be confirmed. Retry to end this session.');
	await expect.element(page.getByText('private-owner', { exact: true })).not.toBeInTheDocument();
	expect(goto).not.toHaveBeenCalled();
	fixture.failDelete('none');
	await page.getByRole('button', { name: 'Sign out' }).click();
	await expect.poll(() => vi.mocked(goto).mock.calls).toEqual([['/sign-in']]);
	expect(fixture.deletes()).toBe(2);
	expect(onlineManager.isOnline()).toBe(false);
	await expect(status()).rejects.toMatchObject({ status: 401, code: 'credential_invalid' });
});

for (const state of ['starting', 'recovering']) {
	test(`${state} logout remains unconfirmed until an authoritative ready retry revokes the persisted credential`, async () => {
		const fixture = sessionHTTP();
		await renderPage();
		await expect.element(page.getByText('private-owner', { exact: true })).toBeVisible();
		fixture.state(state);
		await page.getByRole('button', { name: 'Sign out' }).click();
		await expect
			.element(page.getByRole('status'))
			.toHaveTextContent('Sign-out could not be confirmed. Retry to end this session.');
		await expect.element(page.getByText('private-owner', { exact: true })).not.toBeInTheDocument();
		expect(goto).not.toHaveBeenCalled();
		await expect
			.element(page.getByRole('link', { name: 'Sign in', exact: true }))
			.toHaveAttribute('href', '/sign-in');
		await expect(status()).rejects.toMatchObject({ status: 401, code: 'unauthenticated' });
		fixture.state('ready');
		expect((await status()).state).toBe('ready');
		expect(goto).not.toHaveBeenCalled();
		await page.getByRole('button', { name: 'Sign out' }).click();
		await expect.poll(() => vi.mocked(goto).mock.calls).toEqual([['/sign-in']]);
		await expect(status()).rejects.toMatchObject({ status: 401, code: 'credential_invalid' });
	});
}

for (const outcome of [
	'expired',
	'absent',
	'lost',
	'transport',
	'server',
	'unexpected401'
] as const) {
	test(`sign-out reconciles ${outcome} without private paint and retains real failure recovery`, async () => {
		const fixture = sessionHTTP();
		await renderPage();
		await expect.element(page.getByText('private-owner', { exact: true })).toBeVisible();
		if (outcome === 'expired' || outcome === 'absent')
			fixture.revoke(outcome === 'absent' ? 'unauthenticated' : 'credential_invalid');
		else fixture.failDelete(outcome);
		await page.getByRole('button', { name: 'Sign out' }).click();
		if (outcome === 'absent') {
			await expect
				.element(page.getByRole('status'))
				.toHaveTextContent('Sign-out could not be confirmed. Retry to end this session.');
			expect(goto).not.toHaveBeenCalled();
			await expect
				.element(page.getByText('private-owner', { exact: true }))
				.not.toBeInTheDocument();
			await expect
				.element(page.getByRole('link', { name: 'Sign in', exact: true }))
				.toHaveAttribute('href', '/sign-in');
			await page.getByRole('button', { name: 'Sign out' }).click();
			await expect.poll(() => fixture.deletes()).toBe(2);
			await expect
				.element(page.getByRole('status'))
				.toHaveTextContent('Sign-out could not be confirmed. Retry to end this session.');
			expect(goto).not.toHaveBeenCalled();
			await expect(status()).rejects.toMatchObject({ status: 401, code: 'unauthenticated' });
			return;
		}
		if (outcome !== 'expired') {
			await expect
				.element(page.getByRole('status'))
				.toHaveTextContent('Sign-out could not be confirmed. Retry to end this session.');
			await expect
				.element(page.getByText('private-owner', { exact: true }))
				.not.toBeInTheDocument();
			expect(goto).not.toHaveBeenCalled();
			await expect
				.element(page.getByRole('link', { name: 'Sign in', exact: true }))
				.toHaveAttribute('href', '/sign-in');
			fixture.failDelete('none');
			await page.getByRole('button', { name: 'Sign out' }).click();
		}
		await expect.poll(() => vi.mocked(goto).mock.calls).toEqual([['/sign-in']]);
		await expect.element(page.getByText('private-owner', { exact: true })).not.toBeInTheDocument();
		expect(fixture.deletes()).toBe(outcome === 'expired' ? 1 : 2);
		await expect(status()).rejects.toMatchObject({ status: 401, code: 'credential_invalid' });
	});
}

test('first paint is the public identity with an announced loading status and nothing private', async () => {
	const { release } = holdLifecycle();
	setOnLine(true);
	await renderPage();

	await expect.element(page.getByRole('heading', { level: 1, name: 'Boxset' })).toBeVisible();
	const status = page.getByRole('status');
	await expect.element(status).toHaveTextContent('Loading Boxset…');
	expect(status.element().getAttribute('role')).toBe('status');
	expect(document.querySelector('form, input, textarea, select, a')).toBeNull();
	await publicPending();
	release();
	await required();
});

for (const mode of ['invalid-cue', 'disposal-failure', 'no-body'] as const) {
	test(`lifecycle ${mode} disposes safely and never manufactures private authority`, async () => {
		let cancelled = 0;
		http.fixture.handle = async (input) => {
			const path = new URL(input instanceof Request ? input.url : String(input), location.href)
				.pathname;
			calls.push(path);
			if (path === '/ping') {
				const body =
					mode === 'no-body'
						? null
						: new ReadableStream({
								cancel() {
									cancelled++;
									if (mode === 'disposal-failure') throw new Error('synthetic disposal failure');
								}
							});
				return new Response(body, {
					headers: {
						'X-Boxset-State': mode === 'invalid-cue' ? 'ready,draining' : 'ready',
						'Cache-Control': 'no-store'
					}
				});
			}
			if (path === '/api/v1/session')
				return Response.json({ status: 401, code: 'unauthenticated' }, { status: 401 });
			throw new Error('unexpected protected request');
		};
		setOnLine(true);
		await renderPage();
		if (mode === 'no-body') {
			await required();
			expect(cancelled).toBe(0);
		} else {
			await expect
				.element(page.getByRole('status'))
				.toHaveTextContent(
					'Boxset cannot be reached from this device. Check the connection, then try again.'
				);
			expect(calls).toEqual(['/ping']);
			expect(cancelled).toBe(1);
		}
		expect(document.querySelector('form, input')).toBeNull();
		await expect.element(page.getByRole('button', { name: 'Sign out' })).not.toBeInTheDocument();
	});
}

test('offline shows the reconnect message and a keyboard-reachable Retry that re-checks connectivity', async () => {
	const { release } = holdLifecycle();
	setOnLine(false);
	await renderPage();
	expect(calls).toEqual([]);

	await expect.element(page.getByRole('heading', { name: 'You are offline' })).toBeVisible();
	await expect
		.element(page.getByRole('status'))
		.toHaveTextContent(
			'Boxset cannot be reached from this device. Check the connection, then try again.'
		);

	await userEvent.tab();
	const retry = page.getByRole('button', { name: 'Retry' });
	expect(retry.element().tagName).toBe('BUTTON');
	expect(navigator.userAgent).toContain('HeadlessChrome');
	await expect.element(retry).toHaveFocus();
	expect(retry.element().matches(':focus-visible')).toBe(true);
	expect(getComputedStyle(retry.element()).outlineStyle).not.toBe('none');

	await retry.click();
	await expect.element(page.getByRole('heading', { name: 'You are offline' })).toBeVisible();

	// Still offline: Retry re-checks and the message stays.
	await userEvent.keyboard('{Enter}');
	await expect.element(page.getByRole('heading', { name: 'You are offline' })).toBeVisible();
	await expect.element(retry).toHaveFocus();
	expect(retry.element().matches(':focus-visible')).toBe(true);
	expect(getComputedStyle(retry.element()).outlineStyle).not.toBe('none');

	setOnLine(true);
	await userEvent.keyboard('{Enter}');
	await expect.element(page.getByRole('status')).toHaveTextContent('Loading Boxset…');
	await expect.element(page.getByRole('button', { name: 'Retry' })).not.toBeInTheDocument();
	await publicPending();
	release();
	await required();
});

test('pointer Retry re-checks connectivity without an online event', async () => {
	const { release } = holdLifecycle();
	setOnLine(false);
	await renderPage();
	expect(calls).toEqual([]);
	const retry = page.getByRole('button', { name: 'Retry' });
	await expect.element(retry).toBeVisible();

	setOnLine(true);
	await retry.click();
	await expect.element(page.getByRole('status')).toHaveTextContent('Loading Boxset…');
	await expect.element(retry).not.toBeInTheDocument();
	await publicPending();
	release();
	await required();
});

test('a connectivity change after first paint swaps the public state', async () => {
	const { release } = holdLifecycle();
	setOnLine(true);
	await renderPage();
	await expect.element(page.getByRole('status')).toHaveTextContent('Loading Boxset…');

	setOnLine(false);
	window.dispatchEvent(new Event('offline'));
	await expect.element(page.getByRole('button', { name: 'Retry' })).toBeVisible();

	setOnLine(true);
	window.dispatchEvent(new Event('online'));
	await expect.element(page.getByRole('status')).toHaveTextContent('Loading Boxset…');
	await expect.element(page.getByRole('button', { name: 'Retry' })).not.toBeInTheDocument();
	await publicPending();
	release();
	await expect.element(page.getByRole('status')).toHaveTextContent('Sign in to check this Boxset.');
	expect(calls).toEqual(['/ping', '/ping', '/api/v1/session']);
});

for (const [state, message] of [
	['starting', 'Boxset is starting. Wait a moment, then retry.'],
	['recovering', 'Boxset is recovering. Check the server before trying again.'],
	['draining', 'Boxset is stopping. Wait for it to restart before signing in.']
]) {
	test(`${state} stays public without querying a session`, async () => {
		const { release } = holdLifecycle(state);
		setOnLine(true);
		await renderPage();
		await publicPending();
		release();
		await expect.element(page.getByRole('status')).toHaveTextContent(message);
		expect(calls).toEqual(['/ping']);
		expect(document.querySelector('form, input, a')).toBeNull();
	});
}

test('a confirmed long username stays inside the public shell width', async () => {
	const username = 'W'.repeat(64);
	const user = {
		id: 1,
		username,
		role: 'admin',
		disabled: false,
		revision: 1,
		createdAt: '2026-10-24T00:00:00Z'
	};
	http.fixture.handle = async (input) => {
		const path = new URL(input instanceof Request ? input.url : String(input), location.href)
			.pathname;
		if (path === '/ping') return new Response('ok', { headers: { 'X-Boxset-State': 'ready' } });
		if (path === '/api/v1/session')
			return Response.json({
				user,
				rights: ['status:read', 'users:read', 'users:write'],
				credential: 'cookie',
				restoreEpoch: 1,
				expiresAt: '2026-11-23T00:00:00Z',
				localBypassEnabled: false,
				canLogout: true
			});
		if (path === '/api/v1/system/status')
			return Response.json({
				state: 'ready',
				version: 'test',
				schemaVersion: 4,
				restoreEpoch: 1,
				startedAt: '2026-10-24T00:00:00Z',
				timings: { migrationMs: 0, recoveryMs: 0 },
				goMemLimitBytes: 1048576
			});
		throw new Error('unexpected request');
	};
	setOnLine(true);
	await renderPage();
	await expect.element(page.getByRole('status')).toHaveTextContent('Boxset is ready.');
	const name = page.getByText(username, { exact: true });
	await expect.element(name).toBeVisible();
	expect(name.element().scrollWidth).toBeLessThanOrEqual(name.element().clientWidth);
});

// A bypass-capable server answers the session without a user, and a cookie
// beside it is still the logout authority.
function bypassHTTP(kind: 'bypass' | 'cookie') {
	const snapshot = {
		state: 'ready',
		version: 'test',
		schemaVersion: 4,
		restoreEpoch: 1,
		startedAt: '2026-10-24T00:00:00Z',
		timings: { migrationMs: 0, recoveryMs: 0 },
		goMemLimitBytes: 1048576
	};
	const user = {
		id: 1,
		username: 'private-owner',
		role: 'admin',
		revision: 1,
		disabled: false,
		createdAt: '2026-10-24T00:00:00Z'
	};
	const rights = ['status:read', 'users:read', 'users:write'];
	const enabled = true;
	let fail = false;
	let deletes = 0;
	let hold = false;
	const holds: Array<() => void> = [];
	http.fixture.handle = async (input, init) => {
		const request =
			input instanceof Request ? input : new Request(new URL(String(input), location.href), init);
		const path = new URL(request.url).pathname;
		calls.push(path);
		if (path === '/ping')
			return new Response('ok', {
				headers: { 'X-Boxset-State': 'ready', 'Cache-Control': 'no-store' }
			});
		if (request.method === 'DELETE') {
			deletes++;
			if (fail) return Response.json({ status: 503, code: 'draining' }, { status: 503 });
			return new Response(null, { status: 204 });
		}
		if (path === '/api/v1/session') {
			if (hold)
				await new Promise<void>((release) => {
					holds.push(release);
					pending.push(release);
				});
			return Response.json(
				kind === 'bypass'
					? {
							rights,
							credential: 'local_bypass',
							restoreEpoch: 1,
							localBypassEnabled: enabled,
							canLogout: false
						}
					: {
							user,
							rights,
							credential: 'cookie',
							restoreEpoch: 1,
							expiresAt: '2026-11-23T00:00:00Z',
							localBypassEnabled: enabled,
							canLogout: true
						}
			);
		}
		if (path === '/api/v1/system/status') return Response.json(snapshot);
		throw new Error('unexpected request');
	};
	setOnLine(true);
	setVisible(true);
	return {
		fail: () => {
			fail = true;
		},
		deletes: () => deletes,
		hold: () => {
			hold = true;
		},
		holds
	};
}

test('bypass-only access shows the local label, no user, no sign-out and keeps sign-in', async () => {
	bypassHTTP('bypass');
	await renderPage();
	await expect
		.element(page.getByRole('heading', { name: 'This network is trusted without signing in' }))
		.toBeVisible();
	await expect.element(page.getByRole('status')).toHaveTextContent('Boxset is ready.');
	await expect
		.element(page.getByText('Local access without sign-in', { exact: true }))
		.toBeVisible();
	expect(document.body.textContent).not.toContain('private-owner');
	await expect.element(page.getByRole('button', { name: 'Sign out' })).not.toBeInTheDocument();
	await expect
		.element(page.getByRole('link', { name: 'Sign in' }))
		.toHaveAttribute('href', '/sign-in');
});

test('the bypass warning stays mounted through offline, pending and a failed sign-out', async () => {
	const fixture = bypassHTTP('cookie');
	await renderPage();
	const warning = page.getByRole('heading', { name: 'This network is trusted without signing in' });
	await expect.element(warning).toBeVisible();
	await expect.element(page.getByText('private-owner', { exact: true })).toBeVisible();

	setOnLine(false);
	window.dispatchEvent(new Event('offline'));
	await expect.element(warning).toBeVisible();
	expect(document.body.textContent).not.toContain('private-owner');

	setOnLine(true);
	fixture.hold();
	window.dispatchEvent(new Event('online'));
	await expect.poll(() => fixture.holds.length).toBe(1);
	await expect.element(page.getByRole('status')).toHaveTextContent('Loading Boxset…');
	await expect.element(warning).toBeVisible();
	expect(document.body.textContent).not.toContain('private-owner');

	fixture.holds[0]();
	await expect.element(page.getByText('private-owner', { exact: true })).toBeVisible();
	fixture.fail();
	await page.getByRole('button', { name: 'Sign out' }).click();
	await expect.poll(() => fixture.deletes()).toBe(1);
	await expect
		.element(page.getByRole('status'))
		.toHaveTextContent('Sign-out could not be confirmed. Retry to end this session.');
	await expect.element(warning).toBeVisible();
});
