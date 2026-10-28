import type { Snippet } from 'svelte';
import { afterAll, afterEach, beforeEach, expect, test, vi } from 'vitest';
import { page, userEvent } from 'vitest/browser';
import { cleanup, render } from 'vitest-browser-svelte';
import { onlineManager } from '@tanstack/svelte-query';
import { goto } from '$app/navigation';
import Layout from '../+layout.svelte';
import Page from './+page.svelte';
import { ApiError, login } from '../../lib/api';

const http = vi.hoisted(() => {
	const original = globalThis.fetch.bind(globalThis);
	const fixture: { handle?: typeof fetch } = {};
	globalThis.fetch = (...args) => {
		const input = args[0];
		const path = new URL(input instanceof Request ? input.url : String(input), location.href)
			.pathname;
		if (fixture.handle && path.endsWith('/api/v1/session')) return fixture.handle(...args);
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
afterEach(async () => {
	await cleanup();
	http.fixture.handle = undefined;
	onlineManager.setOnline(true);
	Reflect.deleteProperty(navigator, 'onLine');
	vi.mocked(goto).mockReset();
});

vi.mock('../../lib/api', async (original) => ({
	...(await original<typeof import('../../lib/api')>()),
	login: vi.fn()
}));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
const loginMock = vi.mocked(login);
beforeEach(() => {
	loginMock.mockReset();
});

async function renderPage() {
	await render(Layout, { props: { children: Page as unknown as Snippet } });
}
async function fill() {
	await userEvent.fill(page.getByLabelText('Username'), 'owner');
	await userEvent.fill(page.getByLabelText('Password', { exact: true }), 'synthetic-long-password');
}

for (const accepted of [true, false, 'transport'] as const) {
	test(`fresh browser connectivity submits through the composed typed client with stale manager: accepted=${accepted}`, async () => {
		Object.defineProperty(navigator, 'onLine', { configurable: true, get: () => true });
		onlineManager.setOnline(false);
		let posts = 0;
		http.fixture.handle = async (input, init) => {
			const request =
				input instanceof Request ? input : new Request(new URL(String(input), location.href), init);
			expect(request.method).toBe('POST');
			const credentials = await request.json();
			expect(
				credentials.username === 'owner' && credentials.password === 'synthetic-long-password'
			).toBe(true);
			posts++;
			if (accepted === 'transport') throw new TypeError('synthetic transport failure');
			if (!accepted)
				return Response.json({ status: 401, code: 'credentials_invalid' }, { status: 401 });
			return Response.json(
				{
					user: {
						id: 1,
						username: 'owner',
						role: 'admin',
						disabled: false,
						revision: 1,
						createdAt: '2026-10-24T00:00:00Z'
					},
					expiresAt: '2026-11-23T00:00:00Z'
				},
				{ status: 201 }
			);
		};
		const actual = await vi.importActual<typeof import('../../lib/api')>('../../lib/api');
		loginMock.mockImplementation(actual.login);
		await renderPage();
		await fill();
		await page.getByRole('button', { name: 'Sign in', exact: true }).click();
		await expect.poll(() => posts).toBe(1);
		expect(onlineManager.isOnline()).toBe(false);
		await expect.element(page.getByLabelText('Password', { exact: true })).toHaveValue('');
		if (accepted === true) await expect.poll(() => vi.mocked(goto).mock.calls).toEqual([['/']]);
		else {
			await expect
				.element(page.getByRole('status'))
				.toHaveTextContent(
					accepted === 'transport'
						? 'Boxset cannot be reached from this device. Check the connection, then try again.'
						: 'The username or password is incorrect. Try again.'
				);
			expect(goto).not.toHaveBeenCalled();
		}
	});
}

test('empty input is linked to the field and does not submit', async () => {
	await renderPage();
	await userEvent.click(page.getByRole('button', { name: 'Sign in' }));
	await expect.element(page.getByLabelText('Username')).toHaveAttribute('aria-invalid', 'true');
	await expect
		.element(page.getByLabelText('Password', { exact: true }))
		.toHaveAttribute('aria-invalid', 'true');
	expect(loginMock).not.toHaveBeenCalled();
});

test('wrong credentials clear the password, retain username and allow retry', async () => {
	loginMock.mockRejectedValue(new ApiError(401, 'credentials_invalid'));
	await renderPage();
	await fill();
	await userEvent.click(page.getByRole('button', { name: 'Sign in' }));
	await expect
		.element(page.getByRole('status'))
		.toHaveTextContent('The username or password is incorrect. Try again.');
	await expect.element(page.getByLabelText('Username')).toHaveValue('owner');
	await expect.element(page.getByLabelText('Password', { exact: true })).toHaveValue('');
	await expect.element(page.getByRole('button', { name: 'Sign in' })).toBeEnabled();
	await fill();
	await userEvent.click(page.getByRole('button', { name: 'Sign in' }));
	expect(loginMock).toHaveBeenCalledTimes(2);
});

test('pending sign-in prevents duplicate submission and announces progress', async () => {
	let reject!: (error: unknown) => void;
	loginMock.mockImplementation(
		() =>
			new Promise((_, fail) => {
				reject = fail;
			})
	);
	await renderPage();
	await fill();
	await userEvent.click(page.getByRole('button', { name: 'Sign in' }));
	await expect.element(page.getByRole('button', { name: 'Sign in' })).toBeDisabled();
	await expect.element(page.getByRole('status')).toHaveTextContent('Signing in…');
	expect(loginMock).toHaveBeenCalledTimes(1);
	reject(new ApiError(0, 'offline'));
	await expect
		.element(page.getByRole('status'))
		.toHaveTextContent(
			'Boxset cannot be reached from this device. Check the connection, then try again.'
		);
});

for (const [code, message] of [
	['offline', 'Boxset cannot be reached from this device. Check the connection, then try again.'],
	['hash_busy', 'Boxset is busy. Try again in a moment.'],
	['login_throttled', 'Boxset is busy. Try again in a moment.'],
	['host_not_allowed', 'Open Boxset by its IP address or an allowed host name, then try again.'],
	[
		'cross_origin_denied',
		'This request came from another site. Open Boxset directly and try again.'
	],
	['starting', 'Boxset is not ready to accept the admin yet. Try again in a moment.'],
	['recovering', 'Boxset is not ready to accept the admin yet. Try again in a moment.'],
	['draining', 'Boxset is not ready to accept the admin yet. Try again in a moment.']
]) {
	test(`${code} provides translated recovery feedback`, async () => {
		loginMock.mockRejectedValue(new ApiError(503, code));
		await renderPage();
		await fill();
		await userEvent.click(page.getByRole('button', { name: 'Sign in' }));
		await expect.element(page.getByRole('status')).toHaveTextContent(message);
		await expect.element(page.getByLabelText('Password', { exact: true })).toHaveValue('');
	});
}
