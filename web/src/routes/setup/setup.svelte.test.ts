import type { Snippet } from 'svelte';
import { afterAll, afterEach, beforeEach, expect, test, vi } from 'vitest';
import { page, userEvent } from 'vitest/browser';
import { render } from 'vitest-browser-svelte';
import { ApiError, setup, type Credentials, type User } from '../../lib/api';
import Layout from '../+layout.svelte';
import Page from './+page.svelte';

const http = vi.hoisted(() => {
	const original = globalThis.fetch.bind(globalThis);
	const fixture: { response?: Response } = {};
	globalThis.fetch = (...args) => {
		const input = args[0];
		const url = input instanceof Request ? input.url : String(input);
		if (fixture.response && new URL(url, location.href).pathname.endsWith('/api/v1/setup')) {
			return Promise.resolve(fixture.response.clone());
		}
		return original(...args);
	};
	return {
		fixture,
		restore: () => {
			globalThis.fetch = original;
		}
	};
});

vi.mock('../../lib/api', async (original) => ({
	...(await original<typeof import('../../lib/api')>()),
	setup: vi.fn()
}));
const setupMock = vi.mocked(setup);

const TOKEN = 'tok-0123456789abcdefghijklmnopqrstuvwxyz-ABCDEFG';
const PASSWORD = 'correct horse battery staple';
const admin: User = {
	id: 1,
	username: 'owner',
	role: 'admin',
	disabled: false,
	revision: 1,
	createdAt: '2026-10-23T12:00:00Z'
};

// The page under its real layout, so the query client comes from the app's provider.
async function renderPage() {
	await render(Layout, { props: { children: Page as unknown as Snippet } });
}

async function fill(token: string, username: string, password: string) {
	await userEvent.fill(page.getByLabelText('Setup token'), token);
	await userEvent.fill(page.getByLabelText('Username'), username);
	await userEvent.fill(page.getByLabelText('Password', { exact: true }), password);
}

beforeEach(() => {
	setupMock.mockReset();
	document.documentElement.dataset.boxsetEnv = 'container';
});

afterEach(() => {
	delete document.documentElement.dataset.boxsetEnv;
	delete http.fixture.response;
});

afterAll(http.restore);

test('first access names the token command for the detected environment and labels every field', async () => {
	await renderPage();
	await expect
		.element(page.getByRole('heading', { level: 1, name: 'Create the first admin' }))
		.toBeVisible();
	await expect
		.element(page.getByText('docker exec -it <container> /boxset setup-token'))
		.toBeVisible();
	const token = page.getByLabelText('Setup token');
	expect(token.element().getAttribute('type')).toBe('password');
	expect(token.element().getAttribute('autocomplete')).toBe('off');
	expect(page.getByLabelText('Username').element().getAttribute('autocomplete')).toBe('username');
	const password = page.getByLabelText('Password', { exact: true });
	expect(password.element().getAttribute('autocomplete')).toBe('new-password');
	expect(password.element().getAttribute('aria-describedby')).toBe('setup-password-description');
	await expect.element(page.getByRole('button', { name: 'Create admin' })).toBeEnabled();
	expect(setupMock).not.toHaveBeenCalled();
});

test('a native install names the plain command', async () => {
	document.documentElement.dataset.boxsetEnv = 'native';
	await renderPage();
	await expect.element(page.getByText('boxset setup-token', { exact: true })).toBeVisible();
});

test('invalid input is reported per field, linked to it, and focuses the first invalid field', async () => {
	await renderPage();
	await userEvent.fill(page.getByLabelText('Password', { exact: true }), 'short');
	await userEvent.click(page.getByRole('button', { name: 'Create admin' }));

	const token = page.getByLabelText('Setup token');
	await expect.element(page.getByText('Paste the setup token.')).toBeVisible();
	await expect.element(page.getByText('Enter a username.')).toBeVisible();
	await expect
		.element(
			page.getByText(
				'Use between 12 and 1024 bytes. Letters outside the English alphabet can use more than one byte.'
			)
		)
		.toBeVisible();
	expect(token.element().getAttribute('aria-invalid')).toBe('true');
	expect(token.element().getAttribute('aria-describedby')).toBe('setup-token-error');
	expect(
		page.getByLabelText('Password', { exact: true }).element().getAttribute('aria-describedby')
	).toBe('setup-password-description setup-password-error');
	await expect.element(token).toHaveFocus();
	expect(setupMock).not.toHaveBeenCalled();

	// More than 1024 bytes in fewer than 1024 characters is still too long.
	await fill(TOKEN, 'owner', 'é'.repeat(600));
	await userEvent.click(page.getByRole('button', { name: 'Create admin' }));
	await expect
		.element(
			page.getByText(
				'Use between 12 and 1024 bytes. Letters outside the English alphabet can use more than one byte.'
			)
		)
		.toBeVisible();
	await expect.element(page.getByText('Paste the setup token.')).not.toBeInTheDocument();
	expect(setupMock).not.toHaveBeenCalled();
});

test('a pending submission is announced, cannot be sent twice, and success names the admin', async () => {
	let finish: (u: User) => void = () => {};
	setupMock.mockImplementation(() => new Promise<User>((resolve) => (finish = resolve)));
	await renderPage();
	await fill(TOKEN, 'owner', PASSWORD);
	const submit = page.getByRole('button', { name: 'Create admin' });
	await userEvent.click(submit);

	await expect.element(page.getByRole('status')).toHaveTextContent('Creating the admin…');
	await expect.element(submit).toBeDisabled();
	await userEvent.keyboard('{Enter}');
	expect(setupMock).toHaveBeenCalledTimes(1);
	expect(setupMock).toHaveBeenCalledWith(TOKEN, {
		username: 'owner',
		password: PASSWORD
	} satisfies Credentials);

	finish(admin);
	await expect
		.element(page.getByRole('heading', { level: 1, name: 'Admin created' }))
		.toBeVisible();
	await expect
		.element(page.getByRole('status'))
		.toHaveTextContent(
			'owner is the admin of this Boxset. Sign in with this username and password.'
		);
	expect(document.querySelector('input')).toBeNull();
	expect(document.body.textContent).not.toContain(TOKEN);
});

test('a wrong or replayed token keeps the username, clears the password and says how to get the token', async () => {
	setupMock.mockRejectedValueOnce(new ApiError(401, 'setup_credential_invalid'));
	await renderPage();
	await fill(TOKEN, 'owner', PASSWORD);
	await userEvent.click(page.getByRole('button', { name: 'Create admin' }));

	await expect
		.element(page.getByRole('status'))
		.toHaveTextContent(
			'That setup token is not valid. Print it again with the command above and paste the whole token.'
		);
	await expect.element(page.getByLabelText('Username')).toHaveValue('owner');
	await expect.element(page.getByLabelText('Password', { exact: true })).toHaveValue('');
	expect(document.body.textContent).not.toContain(TOKEN);
	await expect.element(page.getByRole('button', { name: 'Create admin' })).toBeEnabled();
});

test('setup already complete offers no way to reopen it', async () => {
	setupMock.mockRejectedValueOnce(new ApiError(409, 'setup_completed'));
	await renderPage();
	await fill(TOKEN, 'owner', PASSWORD);
	await userEvent.click(page.getByRole('button', { name: 'Create admin' }));

	await expect
		.element(page.getByRole('heading', { level: 1, name: 'Setup is already complete' }))
		.toBeVisible();
	await expect.element(page.getByRole('link', { name: 'Open Boxset' })).toBeVisible();
	expect(document.querySelector('form, input')).toBeNull();
});

test('password bounds count UTF-8 bytes, including non-ASCII input', async () => {
	setupMock.mockResolvedValueOnce(admin);
	await renderPage();
	await fill(TOKEN, 'owner', 'é'.repeat(6));
	await userEvent.click(page.getByRole('button', { name: 'Create admin' }));
	await expect.element(page.getByRole('heading', { name: 'Admin created' })).toBeVisible();
	expect(setupMock).toHaveBeenCalledWith(TOKEN, { username: 'owner', password: 'é'.repeat(6) });
});

test('server username validation is translated, linked and focused without echoing input', async () => {
	setupMock.mockRejectedValueOnce(new ApiError(422, 'validation_failed', ['username']));
	await renderPage();
	await fill(TOKEN, 'hidden' + String.fromCodePoint(0x202e) + 'format', PASSWORD);
	await userEvent.click(page.getByRole('button', { name: 'Create admin' }));
	const username = page.getByLabelText('Username');
	await expect
		.element(page.getByText('Use 1 to 64 characters without control or formatting characters.'))
		.toBeVisible();
	await expect.element(username).toHaveFocus();
	expect(username.element().getAttribute('aria-invalid')).toBe('true');
	expect(username.element().getAttribute('aria-describedby')).toBe('setup-username-error');
	await expect.element(page.getByLabelText('Password', { exact: true })).toHaveValue('');
	await userEvent.fill(page.getByLabelText('Password', { exact: true }), PASSWORD);
	await expect.element(username).toHaveAttribute('aria-invalid', 'true');
	await expect
		.element(page.getByText('Use 1 to 64 characters without control or formatting characters.'))
		.toBeVisible();
	await userEvent.click(page.getByRole('button', { name: 'Create admin' }));
	await expect.element(username).toHaveFocus();
	expect(setupMock).toHaveBeenCalledTimes(1);
	await userEvent.fill(username, 'corrected-owner');
	await expect.element(username).not.toHaveAttribute('aria-invalid', 'true');
	setupMock.mockResolvedValueOnce({ ...admin, username: 'corrected-owner' });
	await userEvent.click(page.getByRole('button', { name: 'Create admin' }));
	await expect.element(page.getByRole('heading', { name: 'Admin created' })).toBeVisible();
	expect(setupMock).toHaveBeenCalledTimes(2);
});

test('username bounds count Unicode codepoints including supplementary characters', async () => {
	setupMock.mockResolvedValueOnce(admin);
	await renderPage();
	await fill(TOKEN, '😀'.repeat(65), PASSWORD);
	await userEvent.click(page.getByRole('button', { name: 'Create admin' }));
	const username = page.getByLabelText('Username');
	await expect.element(page.getByText('Use at most 64 characters.')).toBeVisible();
	await expect.element(username).toHaveFocus();
	expect(username.element().getAttribute('aria-describedby')).toBe('setup-username-error');
	expect(setupMock).not.toHaveBeenCalled();
	await userEvent.fill(username, '😀'.repeat(64));
	await userEvent.click(page.getByRole('button', { name: 'Create admin' }));
	await expect.element(page.getByRole('heading', { name: 'Admin created' })).toBeVisible();
	expect(setupMock).toHaveBeenCalledWith(TOKEN, { username: '😀'.repeat(64), password: PASSWORD });
});

test('each other failure has its own plain message', async () => {
	const cases: [ApiError, string][] = [
		[
			new ApiError(429, 'setup_throttled'),
			'Too many attempts with a wrong token. Wait a few minutes, then try again.'
		],
		[new ApiError(429, 'hash_busy'), 'Boxset is busy. Try again in a moment.'],
		[
			new ApiError(421, 'host_not_allowed'),
			'Setup cannot be reached through this address. Open Boxset by its IP address or by localhost, or add this name to BOXSET_ALLOWED_HOSTS.'
		],
		[
			new ApiError(503, 'draining'),
			'Boxset is not ready to accept the admin yet. Try again in a moment.'
		],
		[
			new ApiError(0, 'offline'),
			'Boxset cannot be reached from this device. Check the connection, then try again.'
		],
		[new ApiError(500, 'internal_error'), 'The admin was not created. Try again.']
	];
	for (const [error, message] of cases) {
		setupMock.mockReset();
		setupMock.mockRejectedValueOnce(error);
		const { unmount } = await render(Layout, { props: { children: Page as unknown as Snippet } });
		await fill(TOKEN, 'owner', PASSWORD);
		await userEvent.click(page.getByRole('button', { name: 'Create admin' }));
		await expect.element(page.getByRole('status')).toHaveTextContent(message);
		unmount();
	}
});

test('the typed client retains only known validation fields from a serialized problem', async () => {
	const response = new Response(
		JSON.stringify({
			status: 422,
			title: 'Unprocessable Content',
			code: 'validation_failed',
			errors: [
				{ location: 'body.username', code: 'invalid', message: 'server-message-sentinel' },
				{ location: 'body.username', code: 'invalid', message: 'server-message-sentinel' },
				{ location: 'body.unknown-sentinel', code: 'invalid', value: 'value-sentinel' }
			]
		}),
		{ status: 422, headers: { 'Content-Type': 'application/problem+json' } }
	);
	http.fixture.response = response;
	const actual = await vi.importActual<typeof import('../../lib/api')>('../../lib/api');
	const error = await actual.setup(TOKEN, { username: 'owner', password: PASSWORD }).then(
		() => null,
		(error) => error
	);
	expect(error).toBeInstanceOf(actual.ApiError);
	expect(error.status).toBe(422);
	expect(error.code).toBe('validation_failed');
	expect(error.fields).toEqual(['username']);
	expect(JSON.stringify(error)).not.toContain('sentinel');
});
