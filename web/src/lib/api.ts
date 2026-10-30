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

export type EventHello = components['schemas']['EventHello'];
export type EventsFrame =
	| { id: string; event: 'hello'; data: EventHello }
	| { id: string; event: 'resync'; data: components['schemas']['EventResync'] }
	| { id: string; event: 'job'; data: components['schemas']['EventJob'] }
	| { id: string; event: 'users'; data: components['schemas']['EventUsers'] }
	| { id: string; event: 'system'; data: components['schemas']['EventSystem'] };

// The identity a tab confirms its shared stream with: exactly the values its own
// session response carries, which the stream's hello repeats.
export type EventsIdentity = {
	installId: string;
	userId: number;
	restoreEpoch: number;
};

// The wire's hard bounds (architecture § API and wire contract): one line is at
// most 8,199 raw bytes excluding LF, and the data value's JSON at most 8,192.
const frameLineLimit = 8199;
const frameDataLimit = 8192;
const problemLimit = 8 << 10;

function lineBytes(text: string): number {
	return new TextEncoder().encode(text).length;
}

// boundedText reads at most limit bytes of a response body: a refusal is short,
// and a hostile one is cut instead of accumulated.
async function boundedText(response: Response, limit: number): Promise<string> {
	const body = response.body;
	if (!body) return '';
	const reader = body.getReader();
	const decoder = new TextDecoder();
	const chunks: Uint8Array[] = [];
	let total = 0;
	try {
		while (total < limit) {
			const { value, done } = await reader.read();
			if (done) break;
			if (!value) continue;
			const room = limit - total;
			const chunk = value.length > room ? value.subarray(0, room) : value;
			chunks.push(chunk);
			total += chunk.length;
		}
	} finally {
		void reader.cancel().catch(() => {});
	}
	return chunks.map((chunk) => decoder.decode(chunk, { stream: true })).join('') + decoder.decode();
}

async function boundedProblem(response: Response): Promise<Problem | undefined> {
	try {
		const text = await boundedText(response, problemLimit);
		const problem = JSON.parse(text) as Problem;
		return problem && typeof problem.code === 'string' ? problem : undefined;
	} catch {
		return undefined;
	}
}

// The schema each frame's data must satisfy, from the generated components: a
// frame that names another event or misses a field is malformed, never guessed at.
function frame(event: string, value: unknown): EventsFrame | undefined {
	switch (event) {
		case 'hello': {
			const hello = value as EventHello | undefined;
			if (
				typeof hello?.watermark !== 'number' ||
				typeof hello.installId !== 'string' ||
				typeof hello.restoreEpoch !== 'number' ||
				typeof hello.userId !== 'number' ||
				Array.isArray(hello.rights) !== true
			)
				return undefined;
			return { id: '', event: 'hello', data: hello };
		}
		case 'resync': {
			const resync = value as components['schemas']['EventResync'] | undefined;
			if (typeof resync?.watermark !== 'number' || typeof resync.reason !== 'string')
				return undefined;
			return { id: '', event: 'resync', data: resync };
		}
		case 'job': {
			const job = value as components['schemas']['EventJob'] | undefined;
			if (!job?.job || typeof job.job.id !== 'number') return undefined;
			return { id: '', event: 'job', data: job };
		}
		case 'users': {
			const users = value as components['schemas']['EventUsers'] | undefined;
			if (typeof users?.collectionRevision !== 'number') return undefined;
			return { id: '', event: 'users', data: users };
		}
		case 'system': {
			const system = value as components['schemas']['EventSystem'] | undefined;
			if (typeof system?.state !== 'string') return undefined;
			return { id: '', event: 'system', data: system };
		}
	}
	return undefined;
}

// eventsStream is the one event-stream fetch: the cookie rides with it, the cursor
// is the last frame the caller applied, and every frame is decoded under the wire's
// bounds. A frame that breaks them aborts this fetch instead of dispatching a
// partial one, so the caller resyncs rather than silently skipping.
export async function* eventsStream(
	signal?: AbortSignal,
	lastEventId?: string
): AsyncGenerator<EventsFrame> {
	const headers: Record<string, string> = { Accept: 'text/event-stream' };
	if (lastEventId) headers['Last-Event-ID'] = lastEventId;
	let response: Response;
	try {
		response = await fetch(`${base}/api/v1/events`, {
			credentials: 'same-origin',
			cache: 'no-store',
			headers,
			signal
		});
	} catch {
		throw new ApiError(0, 'offline');
	}
	if (!response.ok) throw failure(response, await boundedProblem(response));
	if (!response.body) throw new ApiError(0, 'offline');

	let id: string | undefined;
	let event: string | undefined;
	let data: string | undefined;
	let pending: number[] = [];
	const decoder = new TextDecoder();
	const malformed = () => new ApiError(0, 'malformed_frame');

	function fields(line: string): [string, string] {
		const colon = line.indexOf(':');
		if (colon < 0) return [line, ''];
		const value = line.slice(colon + 1);
		return [line.slice(0, colon), value.startsWith(' ') ? value.slice(1) : value];
	}

	function apply(line: string): EventsFrame | undefined {
		if (line === '') {
			if (id === undefined && event === undefined && data === undefined) return undefined;
			if (id === undefined || event === undefined || data === undefined) throw malformed();
			let value: unknown;
			try {
				value = JSON.parse(data);
			} catch {
				throw malformed();
			}
			const decoded = frame(event, value);
			if (!decoded) throw malformed();
			return { ...decoded, id } as EventsFrame;
		}
		if (line.startsWith(':')) return undefined;
		const [name, value] = fields(line);
		switch (name) {
			case 'id':
				if (id !== undefined || lineBytes(value) > 128) throw malformed();
				id = value;
				return undefined;
			case 'event':
				if (event !== undefined || lineBytes(value) > 32) throw malformed();
				event = value;
				return undefined;
			case 'data':
				if (data !== undefined || lineBytes(value) > frameDataLimit) throw malformed();
				data = value;
				return undefined;
		}
		// Every other field, and every comment, is discarded at its line end.
		return undefined;
	}

	const reader = response.body.getReader();
	try {
		for (;;) {
			const { value, done } = await reader.read();
			if (done) {
				if (pending.length > 0) throw malformed();
				throw new ApiError(0, 'stream_ended');
			}
			if (!value) continue;
			for (const byte of value) {
				if (byte === 0x0a) {
					// CRLF ends a line too: the carriage return belongs to the terminator.
					if (pending.length > 0 && pending[pending.length - 1] === 0x0d) pending.pop();
					const parsed = apply(decoder.decode(new Uint8Array(pending)));
					pending = [];
					if (parsed) {
						id = event = data = undefined;
						yield parsed;
					}
					continue;
				}
				if (pending.length >= frameLineLimit) throw malformed();
				pending.push(byte);
			}
		}
	} finally {
		void reader.cancel().catch(() => {});
	}
}
