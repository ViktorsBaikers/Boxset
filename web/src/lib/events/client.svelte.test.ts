// The tab client's contract: a frame refetches the projection the route holds and
// nothing else, the refetch waits for the last write to settle (its failure included),
// a changed or lost identity clears that projection, and a tab's own exit leaves the
// installation's stream alone. The worker half is driven through the same message
// protocol, and eventsStream decodes the wire under its bounds. The confirmation
// itself is typed from the generated Session and EventHello, so no triple can be
// invented here.
import { afterEach, beforeEach, expect, test, vi } from 'vitest';
import { QueryClient } from '@tanstack/svelte-query';
import {
	eventsStream,
	type EventHello,
	type EventsFrame,
	type EventsIdentity,
	type Session
} from '../api';
import { leaveEvents, startEvents, stopEvents } from './client';

type Equals<A, B> =
	(<T>() => T extends A ? 1 : 2) extends <T>() => T extends B ? 1 : 2 ? true : false;

// The tab's confirmation is exactly the triple of its own session response, which is
// exactly the identity the stream's hello carries: a drift on either side fails to
// compile, so a fabricated triple is not expressible in this client.
export const confirmation: [
	Equals<EventsIdentity['installId'], NonNullable<Session['installId']>>,
	Equals<EventsIdentity['userId'], NonNullable<Session['user']>['id']>,
	Equals<EventsIdentity['restoreEpoch'], Session['restoreEpoch']>,
	Equals<EventsIdentity['installId'], EventHello['installId']>,
	Equals<EventsIdentity['userId'], EventHello['userId']>,
	Equals<EventsIdentity['restoreEpoch'], EventHello['restoreEpoch']>
] = [true, true, true, true, true, true];

const identity: EventsIdentity = { installId: 'aaaaaaaaaaaa', userId: 1, restoreEpoch: 1 };
const hello: EventHello = {
	watermark: 7,
	installId: identity.installId,
	userId: identity.userId,
	restoreEpoch: identity.restoreEpoch,
	credential: 'cookie',
	rights: ['status:read']
};

type Name = 'access' | 'status' | 'users' | 'other';

let queryClient: QueryClient;
let channel: MessageChannel;
let received: unknown[];
let fetches: Record<Name, number>;

function counts(): Record<Name, number> {
	return { ...fetches };
}

function frame(event: EventsFrame['event'], data: unknown): EventsFrame {
	return { id: '1.boot.1', event, data } as EventsFrame;
}

// A worker's port, seen from the page side of the protocol.
type WorkerPort = {
	postMessage: (message: unknown) => void;
	onmessage: ((event: MessageEvent) => void) | null;
	start: () => void;
};

function workerPort(): WorkerPort & { received: unknown[] } {
	const port = {
		received: [] as unknown[],
		postMessage: (message: unknown) => port.received.push(message),
		onmessage: null as ((event: MessageEvent) => void) | null,
		start: () => {}
	};
	return port;
}

function confirmWith(port: WorkerPort, triple: EventsIdentity = identity): void {
	port.onmessage?.({ data: { confirm: triple } } as MessageEvent);
}

function leavePort(port: WorkerPort): void {
	port.onmessage?.({ data: { leave: true } } as MessageEvent);
}

// readWorker imports the worker module and returns the connect handler it installed.
// The module keeps one stream state for the whole document, so a case leaves every port
// it opened before it finishes: the next case then starts from no confirmed ports.
async function readWorker(): Promise<(event: { ports: WorkerPort[] }) => void> {
	await import('./stream-worker');
	// SAFETY: the worker installs its onconnect on the global object; this is the only
	// side the test document can drive it from.
	return (globalThis as unknown as { onconnect: (event: { ports: WorkerPort[] }) => void })
		.onconnect;
}

function openStream(signal: AbortSignal | undefined, first = hello): Response {
	const encoder = new TextEncoder();
	const body = new ReadableStream<Uint8Array>({
		start(controller) {
			const data = JSON.stringify(first);
			controller.enqueue(encoder.encode(`id: 1.boot.1\nevent: hello\ndata: ${data}\n\n`));
			signal?.addEventListener('abort', () =>
				controller.error(new DOMException('aborted', 'AbortError'))
			);
		}
	});
	return new Response(body, { status: 200 });
}

function refusal(): Response {
	return new Response(JSON.stringify({ code: 'unauthenticated' }), {
		status: 401,
		headers: { 'Content-Type': 'application/problem+json' }
	});
}

function streamed(text: string): Response {
	const encoder = new TextEncoder();
	return new Response(
		new ReadableStream<Uint8Array>({
			start(controller) {
				controller.enqueue(encoder.encode(text));
				controller.close();
			}
		}),
		{ status: 200 }
	);
}

beforeEach(async () => {
	queryClient = new QueryClient({
		defaultOptions: { queries: { retry: false, gcTime: Infinity }, mutations: { retry: false } }
	});
	fetches = { access: 0, status: 0, users: 0, other: 0 };
	// One query per key the test fabricates, including keys no route holds, so an
	// invalidation of a key nothing subscribes to is visible as a bump.
	const keys: Array<[Name, readonly unknown[]]> = [
		['access', ['access', 0]],
		['status', ['status']],
		['users', ['users']],
		['other', ['other']]
	];
	for (const [name, queryKey] of keys) {
		await queryClient.fetchQuery({
			queryKey,
			queryFn: async () => {
				fetches[name] += 1;
				return name;
			}
		});
	}
	channel = new MessageChannel();
	received = [];
	channel.port2.onmessage = (event) => received.push(event.data);
	channel.port2.start();
});

afterEach(() => {
	stopEvents();
	channel.port1.close();
	channel.port2.close();
	queryClient.clear();
	vi.unstubAllGlobals();
	vi.restoreAllMocks();
});

test('startEvents confirms exactly the identity it was handed, once', async () => {
	startEvents(identity, queryClient, channel.port1);
	await expect.poll(() => received.length).toBe(1);
	expect(received[0]).toEqual({ confirm: identity });
	startEvents(identity, queryClient, channel.port1);
	await new Promise((resolve) => setTimeout(resolve, 50));
	expect(received).toHaveLength(1);
});

test('a hello refetches the projection a route holds, and a job frame refetches nothing', async () => {
	startEvents(identity, queryClient, channel.port1);
	channel.port2.postMessage({ type: 'hello', hello });
	await expect.poll(counts).toEqual({ access: 2, status: 1, users: 1, other: 1 });
	channel.port2.postMessage({ type: 'frame', frame: frame('system', { state: 'ready' }) });
	await expect.poll(counts).toEqual({ access: 3, status: 1, users: 1, other: 1 });
	channel.port2.postMessage({ type: 'frame', frame: frame('users', { collectionRevision: 2 }) });
	await expect.poll(counts).toEqual({ access: 4, status: 1, users: 1, other: 1 });
	channel.port2.postMessage({
		type: 'frame',
		frame: frame('job', {
			job: { id: 1, kind: 'test', class: 'optional', lane: 'background', state: 'succeeded' }
		})
	});
	await new Promise((resolve) => setTimeout(resolve, 100));
	expect(counts(), 'a job frame invalidated a key no route holds').toEqual({
		access: 4,
		status: 1,
		users: 1,
		other: 1
	});
});

test('a frame during a write defers the refetch until the last write settles, failure included', async () => {
	let reject!: (error: Error) => void;
	const held = new Promise<never>((_, fail) => {
		reject = fail;
	});
	const mutation = queryClient.getMutationCache().build(queryClient, { mutationFn: () => held });
	const settled = mutation.execute(undefined).catch(() => 'failed');
	await expect.poll(() => queryClient.isMutating()).toBe(1);

	startEvents(identity, queryClient, channel.port1);
	channel.port2.postMessage({ type: 'hello', hello });
	await new Promise((resolve) => setTimeout(resolve, 100));
	expect(counts()).toEqual({ access: 1, status: 1, users: 1, other: 1 });

	reject(new Error('synthetic write failure'));
	await expect.poll(() => queryClient.isMutating()).toBe(0);
	expect(await settled).toBe('failed');
	await expect.poll(counts).toEqual({ access: 2, status: 1, users: 1, other: 1 });
});

test('a changed identity clears the protected projection and confirms nothing new itself', async () => {
	startEvents(identity, queryClient, channel.port1);
	channel.port2.postMessage({ type: 'hello', hello });
	await expect.poll(counts).toEqual({ access: 2, status: 1, users: 1, other: 1 });

	channel.port2.postMessage({ type: 'identity-changed' });
	await expect.poll(counts).toEqual({ access: 3, status: 1, users: 1, other: 1 });
	// The client sends nothing on a hello or a refusal: it refetches and waits for its own
	// fresh session response before it confirms anything again.
	expect(received).toEqual([{ confirm: identity }]);
	// The tab confirms again only from a fresh session response of its own.
	startEvents({ ...identity, installId: 'bbbbbbbbbbbb' }, queryClient, channel.port1);
	await expect
		.poll(() => received.at(-1))
		.toEqual({
			confirm: { ...identity, installId: 'bbbbbbbbbbbb' }
		});
});

test('a lost identity clears the protected projection for the route to redecide', async () => {
	startEvents(identity, queryClient, channel.port1);
	channel.port2.postMessage({ type: 'hello', hello });
	await expect.poll(counts).toEqual({ access: 2, status: 1, users: 1, other: 1 });
	channel.port2.postMessage({ type: 'identity-lost' });
	await expect.poll(counts).toEqual({ access: 3, status: 1, users: 1, other: 1 });
});

test('a lost identity clears the protected projection before its refetch lands', async () => {
	startEvents(identity, queryClient, channel.port1);
	channel.port2.postMessage({ type: 'hello', hello });
	await expect.poll(counts).toEqual({ access: 2, status: 1, users: 1, other: 1 });

	// The refetch reads the cache at its own start: a cleared entry, never the
	// projection the lost identity painted.
	const query = queryClient.getQueryCache().find({ queryKey: ['access', 0] });
	if (!query) throw new Error('the access projection is missing');
	let projected: unknown = 'unset';
	query.setOptions({
		queryKey: ['access', 0],
		queryFn: async () => {
			projected = queryClient.getQueryData(['access', 0]);
			fetches.access += 1;
			return 'access';
		}
	});
	channel.port2.postMessage({ type: 'identity-lost' });
	await expect.poll(counts).toEqual({ access: 3, status: 1, users: 1, other: 1 });
	expect(projected, 'the cleared projection was still in cache during the refetch').toBeUndefined();
});

test('a tab that lost its identity still leaves its port, so the same triple can reset', async () => {
	startEvents(identity, queryClient, channel.port1);
	channel.port2.postMessage({ type: 'hello', hello });
	await expect.poll(counts).toEqual({ access: 2, status: 1, users: 1, other: 1 });
	channel.port2.postMessage({ type: 'identity-lost' });
	// The reset the loss triggers is the proof it was processed before the leave.
	await expect.poll(counts).toEqual({ access: 3, status: 1, users: 1, other: 1 });
	// The lost identity clears the confirmation but not the port the worker holds: the
	// leave still tells the worker to drop it, which is the reset the next confirm needs.
	leaveEvents();
	await expect.poll(() => received.at(-1)).toEqual({ leave: true });
	startEvents(identity, queryClient, channel.port1);
	await expect.poll(() => received.at(-1)).toEqual({ confirm: identity });
});

test('leaving a route drops this tab only, while a settled loss stops every port', async () => {
	startEvents(identity, queryClient, channel.port1);
	await expect.poll(() => received.length).toBe(1);
	leaveEvents();
	await expect.poll(() => received.at(-1)).toEqual({ leave: true });
	// A tab that no longer confirms sends nothing further.
	leaveEvents();
	await new Promise((resolve) => setTimeout(resolve, 50));
	expect(received).toHaveLength(2);
	// Signing in again confirms the same port; only a settled authorization loss stops.
	startEvents(identity, queryClient, channel.port1);
	await expect.poll(() => received.length).toBe(3);
	stopEvents();
	await expect.poll(() => received.at(-1)).toEqual({ stop: true });
});

test('a repeat confirm from the refused port opens no stream; a fresh port or a leave is the reset', async () => {
	const calls: Array<RequestInit | undefined> = [];
	vi.stubGlobal(
		'fetch',
		vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
			calls.push(init);
			return refusal();
		})
	);
	const port = workerPort();
	const second = workerPort();
	const onconnect = await readWorker();
	onconnect({ ports: [port] });

	confirmWith(port);
	await expect.poll(() => calls.length).toBe(1);
	await expect.poll(() => port.received).toContainEqual({ type: 'identity-lost' });

	// The same port repeats the refused generation: the refusal stands.
	confirmWith(port);
	await new Promise((resolve) => setTimeout(resolve, 100));
	expect(calls.length, 'a repeat confirm opened a second stream').toBe(1);

	// A port that never confirmed the generation is the reset, so the stream resumes.
	onconnect({ ports: [second] });
	confirmWith(second);
	await expect.poll(() => calls.length).toBe(2);
	await expect.poll(() => second.received).toContainEqual({ type: 'identity-lost' });

	// The refused port leaves and returns with the same triple: the leave is the reset,
	// so exactly one new stream opens instead of the refusal holding for ever.
	leavePort(port);
	confirmWith(port);
	await expect.poll(() => calls.length, { timeout: 5_000 }).toBe(3);

	leavePort(port);
	leavePort(second);
});

test('one leave keeps the shared stream; the last leave ends the fetch', async () => {
	const signals: Array<AbortSignal | undefined> = [];
	vi.stubGlobal(
		'fetch',
		vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
			signals.push(init?.signal ?? undefined);
			return openStream(init?.signal ?? undefined);
		})
	);
	const first = workerPort();
	const second = workerPort();
	const third = workerPort();
	const onconnect = await readWorker();
	for (const port of [first, second, third]) {
		onconnect({ ports: [port] });
		confirmWith(port);
	}
	await expect.poll(() => first.received).toContainEqual({ type: 'hello', hello });
	expect(signals, 'the three ports opened more than one stream').toHaveLength(1);
	expect(signals[0]?.aborted).toBe(false);

	leavePort(first);
	leavePort(second);
	await new Promise((resolve) => setTimeout(resolve, 50));
	expect(signals, 'a leaving tab opened another stream').toHaveLength(1);
	expect(signals[0]?.aborted, 'a leaving tab ended the shared stream').toBe(false);
	expect(third.received).not.toContainEqual({ type: 'identity-lost' });

	leavePort(third);
	await expect.poll(() => signals[0]?.aborted).toBe(true);
	expect(signals, 'the last leave opened another stream').toHaveLength(1);
});

test('a stop from one confirmed port ends the shared stream for every port', async () => {
	const signals: Array<AbortSignal | undefined> = [];
	vi.stubGlobal(
		'fetch',
		vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
			signals.push(init?.signal ?? undefined);
			return openStream(init?.signal ?? undefined);
		})
	);
	const first = workerPort();
	const second = workerPort();
	const onconnect = await readWorker();
	for (const port of [first, second]) {
		onconnect({ ports: [port] });
		confirmWith(port);
	}
	await expect.poll(() => first.received).toContainEqual({ type: 'hello', hello });
	expect(signals, 'the two ports opened more than one stream').toHaveLength(1);

	// Signing out settles the session every port shares: the running stream ends and
	// each tab clears into its own session decision instead of reconnecting.
	first.onmessage?.({ data: { stop: true } } as MessageEvent);
	await expect.poll(() => signals[0]?.aborted).toBe(true);
	expect(first.received).toContainEqual({ type: 'identity-lost' });
	expect(second.received, 'a stop left a sibling port confirmed').toContainEqual({
		type: 'identity-lost'
	});
	await new Promise((resolve) => setTimeout(resolve, 100));
	expect(signals, 'a signed-out worker reopened the stream').toHaveLength(1);
});

test('a confirm naming another identity ends the stale stream and the reconnect names it', async () => {
	const other: EventsIdentity = {
		installId: identity.installId,
		userId: 2,
		restoreEpoch: identity.restoreEpoch
	};
	const otherHello: EventHello = { ...hello, userId: other.userId, watermark: 8 };
	const signals: Array<AbortSignal | undefined> = [];
	vi.stubGlobal(
		'fetch',
		vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
			signals.push(init?.signal ?? undefined);
			return openStream(init?.signal ?? undefined, signals.length === 1 ? hello : otherHello);
		})
	);
	const onconnect = await readWorker();
	const first = workerPort();
	const second = workerPort();
	onconnect({ ports: [first] });
	confirmWith(first);
	await expect.poll(() => first.received).toContainEqual({ type: 'hello', hello });

	// A fresh session names an identity the live stream's hello does not: the stale
	// stream must end, or this tab stays mute behind the old greeting for ever.
	onconnect({ ports: [second] });
	confirmWith(second, other);
	await expect.poll(() => signals[0]?.aborted).toBe(true);
	await expect.poll(() => signals.length, { timeout: 10_000 }).toBe(2);
	await expect.poll(() => second.received).toContainEqual({ type: 'hello', hello: otherHello });

	leavePort(first);
	leavePort(second);
});

test('eventsStream forwards the cursor and yields frames until a clean end', async () => {
	const calls: Array<RequestInit | undefined> = [];
	vi.stubGlobal(
		'fetch',
		vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
			calls.push(init);
			return streamed('id: 1.boot.5\nevent: system\ndata: {"state":"ready"}\n\n');
		})
	);
	const frames = eventsStream(undefined, '1.boot.4');
	const first = await frames.next();
	expect((calls[0]?.headers as Record<string, string>)['Last-Event-ID']).toBe('1.boot.4');
	expect(first.value).toMatchObject({ id: '1.boot.5', event: 'system' });
	await expect(frames.next()).rejects.toMatchObject({ code: 'stream_ended' });
});

test('eventsStream accepts a line at the 8,199-byte bound and refuses one past it', async () => {
	// An unknown-field line is discarded at its line end but still counted against the
	// wire's raw line bound, which a data value alone cannot reach.
	const at = 'x'.repeat(8199);
	vi.stubGlobal(
		'fetch',
		vi.fn(async () => streamed(`${at}\nid: 1.boot.1\nevent: system\ndata: {"state":"ready"}\n\n`))
	);
	const frames = eventsStream();
	await expect(frames.next()).resolves.toMatchObject({ value: { event: 'system' } });
	await expect(frames.next()).rejects.toMatchObject({ code: 'stream_ended' });

	const over = 'x'.repeat(8200);
	vi.stubGlobal(
		'fetch',
		vi.fn(async () => streamed(`${over}\nid: 1.boot.1\nevent: system\ndata: {"state":"ready"}\n\n`))
	);
	await expect(eventsStream().next()).rejects.toMatchObject({ code: 'malformed_frame' });
});

test('eventsStream refuses a data value past 8,192 bytes and a malformed frame', async () => {
	vi.stubGlobal(
		'fetch',
		vi.fn(async () => streamed(`id: 1.boot.1\nevent: system\ndata: "${'x'.repeat(8191)}"\n\n`))
	);
	await expect(eventsStream().next()).rejects.toMatchObject({ code: 'malformed_frame' });

	vi.stubGlobal(
		'fetch',
		vi.fn(async () => streamed('id: 1.boot.1\nevent: system\ndata: {}\n\n'))
	);
	const malformed = eventsStream();
	await expect(malformed.next()).rejects.toMatchObject({ code: 'malformed_frame' });
});

test('eventsStream treats CRLF as the line end', async () => {
	vi.stubGlobal(
		'fetch',
		vi.fn(async () => streamed('id: 1.boot.1\r\nevent: system\r\ndata: {"state":"ready"}\r\n\r\n'))
	);
	const frames = eventsStream();
	await expect(frames.next()).resolves.toMatchObject({
		value: { id: '1.boot.1', event: 'system' }
	});
	await expect(frames.next()).rejects.toMatchObject({ code: 'stream_ended' });
});

test('eventsStream accepts a data value at the 8,192-byte bound and refuses an unterminated line or a repeated field', async () => {
	const value = `{"state":"${'x'.repeat(8180)}"}`;
	expect(value.length).toBe(8192);
	vi.stubGlobal(
		'fetch',
		vi.fn(async () => streamed(`id: 1.boot.1\nevent: system\ndata: ${value}\n\n`))
	);
	const frames = eventsStream();
	await expect(frames.next()).resolves.toMatchObject({ value: { event: 'system' } });
	await expect(frames.next()).rejects.toMatchObject({ code: 'stream_ended' });

	vi.stubGlobal(
		'fetch',
		vi.fn(async () => streamed('id: 1.boot.1\nevent: system\ndata: {"state":"ready"}'))
	);
	await expect(eventsStream().next()).rejects.toMatchObject({ code: 'malformed_frame' });

	vi.stubGlobal(
		'fetch',
		vi.fn(async () =>
			streamed('id: 1.boot.1\nevent: system\ndata: {"state":"ready"}\ndata: {"state":"ready"}\n\n')
		)
	);
	await expect(eventsStream().next()).rejects.toMatchObject({ code: 'malformed_frame' });
});
