// The tab half of the shared stream. A tab confirms only with the triple its own
// session response carries, and the invalidations it applies go through the app's one
// QueryClient, so a frame becomes the same refetch a rendered route would ask for.
import type { QueryClient } from '@tanstack/svelte-query';
import { resolve } from '$app/paths';
import type { EventsFrame, EventsIdentity, EventHello } from '../api';

type Message =
	| { type: 'hello'; hello: EventHello }
	| { type: 'frame'; frame: EventsFrame }
	| { type: 'identity-changed' }
	| { type: 'identity-lost' };

// One SharedWorker per base: its name is the base, so the root and /media shells of
// one origin never share a stream.
const base = resolve('/').replace(/\/$/, '');

// Each frame kind names the projection it refreshes, and a frame invalidates only a
// key a route holds: today that is the lifecycle/session pair `['access', confirmation]`.
// A job frame names a collection no route subscribes to, so it refetches nothing.
const affected: Record<EventsFrame['event'], readonly string[]> = {
	hello: ['access'],
	resync: ['access'],
	job: [],
	users: ['access'],
	system: ['access']
};
const protectedKeys = new Set(['access']);

let port: MessagePort | undefined;
let client: QueryClient | undefined;
// joined is this tab's port as the worker holds it: a confirmation it has not left
// since. It is the transport presence leave needs, which an identity loss clears from
// confirmed but not from the worker's port map.
let joined = false;
let confirmed: EventsIdentity | undefined;
// The identity the stream last told this tab it no longer names: a projection that
// still holds it must not confirm it again, or the refusal would repeat for ever.
let stale: EventsIdentity | undefined;
let deferred = new Set<string>();

function same(a: EventsIdentity, b: EventsIdentity): boolean {
	return a.installId === b.installId && a.userId === b.userId && a.restoreEpoch === b.restoreEpoch;
}

// flush applies the invalidations the frames collected. A write is in flight, so the
// refetch waits for the last mutation to settle — its failure included — and lands
// once, however many frames arrived meanwhile.
function flush(): void {
	if (!client || deferred.size === 0) return;
	if (client.isMutating() > 0) return;
	const keys = deferred;
	deferred = new Set();
	for (const query of client.getQueryCache().getAll()) {
		if (keys.has(String(query.queryKey[0])))
			void client.invalidateQueries({ queryKey: query.queryKey, refetchType: 'all' });
	}
}

function invalidate(keys: readonly string[]): void {
	if (!client) return;
	for (const key of keys) deferred.add(key);
	flush();
}

// A changed or lost identity clears the protected projections and refetches each from
// its own authority: the entry is reset before the fetch starts, so the previous
// identity's data is never painted while the answer is in flight. The route's own
// session and status reads then decide the existing sign-in, no-permission or ready
// state. Nothing is confirmed here; the layout hands the new identity over once its own
// session response carries it.
function reset(): void {
	if (!client) return;
	deferred = new Set();
	for (const query of client.getQueryCache().getAll()) {
		if (!protectedKeys.has(String(query.queryKey[0]))) continue;
		const { queryFn } = query.options;
		query.reset();
		if (queryFn)
			void client.query({ ...query.options, queryKey: query.queryKey, queryFn }).catch(() => {});
	}
}

function receive(event: MessageEvent): void {
	const message = event.data as Message;
	switch (message.type) {
		case 'hello':
			invalidate(affected.hello);
			return;
		case 'frame':
			invalidate(affected[message.frame.event]);
			return;
		case 'identity-changed':
			stale = confirmed ?? stale;
			confirmed = undefined;
			reset();
			return;
		case 'identity-lost':
			// Nothing is confirmed any more: this tab confirms again only from a fresh
			// session response of its own.
			confirmed = undefined;
			reset();
			return;
	}
}

export function startEvents(
	identity: EventsIdentity,
	queryClient: QueryClient,
	shared?: MessagePort
): void {
	if (client !== queryClient) {
		client = queryClient;
		queryClient.getMutationCache().subscribe(() => flush());
	}
	if (confirmed && port && same(confirmed, identity)) return;
	if (stale && same(stale, identity)) return;
	stale = undefined;
	confirmed = identity;
	if (shared && port !== shared) {
		port = shared;
		port.onmessage = receive;
		port.start();
	} else if (!port) {
		try {
			const worker = new SharedWorker(new URL('./stream-worker.ts', import.meta.url), {
				type: 'module',
				name: base
			});
			port = worker.port;
			port.onmessage = receive;
			port.start();
		} catch {
			// This browser has no shared worker: the page keeps the refetch it already
			// does on focus and resume, and nothing else changes.
			return;
		}
	}
	port.postMessage({ confirm: identity });
	joined = true;
}

// leaveEvents drops this tab's own port and keeps the installation's shared stream:
// leaving a route or failing to end this tab's session is not an authority loss for
// the other tabs. A port the worker already dropped leaves nothing to send.
export function leaveEvents(): void {
	deferred = new Set();
	if (!joined) return;
	joined = false;
	confirmed = undefined;
	stale = undefined;
	port?.postMessage({ leave: true });
}

// stopEvents ends the shared stream for every port: the session this tab confirmed is
// settled as lost, so each tab re-reads its own session instead of reconnecting. A tab
// that never confirmed leaves the stream alone.
export function stopEvents(): void {
	const owned = confirmed !== undefined;
	confirmed = undefined;
	stale = undefined;
	deferred = new Set();
	if (owned && port) port.postMessage({ stop: true });
}
