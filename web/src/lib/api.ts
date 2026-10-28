// The only way the SPA reaches the server: a typed client over the generated OpenAPI types
// (api.gen.ts, from internal/api/openapi.json).
import createClient from 'openapi-fetch';
import { resolve } from '$app/paths';
import type { components, paths } from './api.gen';

export type User = components['schemas']['User'];
export type Credentials = components['schemas']['Credentials'];
export type Problem = components['schemas']['Problem'];
export type Status = components['schemas']['Status'];
export type Session = components['schemas']['Session'];
export type ValidationField = keyof Credentials;

// The runtime base, without a trailing slash ('' at the root).
const base = resolve('/').replace(/\/$/, '');

const client = createClient<paths>({ baseUrl: base });

// A failed call: the problem's code, or `offline` when no response arrived.
export class ApiError extends Error {
	constructor(
		readonly status: number,
		readonly code: string,
		readonly fields: readonly ValidationField[] = []
	) {
		super(code);
	}
}

function failure(response: Response, problem: Problem | undefined): ApiError {
	const fields = new Set<ValidationField>();
	if (Array.isArray(problem?.errors)) {
		for (const error of problem.errors.slice(0, 32)) {
			if (error?.location === 'body.username') fields.add('username');
			if (error?.location === 'body.password') fields.add('password');
		}
	}
	return new ApiError(response.status, problem?.code ?? 'unexpected_response', [...fields]);
}

// Creates the first admin with the setup token. The token travels only in the Authorization
// header and is never kept.
export async function setup(token: string, credentials: Credentials): Promise<User> {
	let result;
	try {
		result = await client.POST('/api/v1/setup', {
			body: credentials,
			headers: { Authorization: `Bearer ${token}` }
		});
	} catch {
		throw new ApiError(0, 'offline');
	}
	if (result.data) return result.data.user;
	throw failure(result.response, result.error);
}

export async function login(credentials: Credentials): Promise<void> {
	let result;
	try {
		result = await client.POST('/api/v1/session', { body: credentials });
	} catch {
		throw new ApiError(0, 'offline');
	}
	if (!result.data) throw failure(result.response, result.error);
}

export async function session(signal?: AbortSignal): Promise<Session> {
	let result;
	try {
		result = await client.GET('/api/v1/session', { cache: 'no-store', signal });
	} catch {
		throw new ApiError(0, 'offline');
	}
	if (result.data) return result.data;
	throw failure(result.response, result.error);
}

export async function logout(): Promise<void> {
	let result;
	try {
		result = await client.DELETE('/api/v1/session');
	} catch {
		throw new ApiError(0, 'offline');
	}
	// Before authority is attached, the guard also returns unauthenticated.
	if (result.response.status === 401 && result.error?.code === 'credential_invalid') return;
	if (result.response.status !== 204) throw failure(result.response, result.error);
}

export async function status(signal?: AbortSignal): Promise<Status> {
	let result;
	try {
		result = await client.GET('/api/v1/system/status', { cache: 'no-store', signal });
	} catch {
		throw new ApiError(0, 'offline');
	}
	if (result.data) return result.data;
	throw failure(result.response, result.error);
}

export async function lifecycle(signal?: AbortSignal): Promise<Status['state']> {
	let response: Response;
	try {
		response = await fetch(`${base}/ping`, { cache: 'no-store', signal });
		await response.body?.cancel();
	} catch {
		throw new ApiError(0, 'offline');
	}
	const state = response.headers.get('X-Boxset-State');
	if (
		response.ok &&
		(state === 'starting' || state === 'recovering' || state === 'ready' || state === 'draining')
	)
		return state;
	throw new ApiError(response.status, 'unexpected_response');
}
