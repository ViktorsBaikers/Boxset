// The one authenticated event stream of an installation base. A module SharedWorker
// named after the base is instantiated once per origin, so every tab of that base
// reads the same server stream; the import graph is api.ts only, because a worker
// resolves no route and touches no document.
import {
	ApiError,
	eventsStream,
	type EventsFrame,
	type EventsIdentity,
	type EventHello
} from '../api';

// The port protocol between this worker and the tabs' client.ts.
type Port = {
	postMessage(message: unknown): void;
	onmessage: ((event: MessageEvent) => void) | null;
	start(): void;
};

const ports = new Map<Port, EventsIdentity | undefined>();
let hello: EventHello | undefined;
let lastEventId: string | undefined;
let stream: AbortController | undefined;
let attempt = 0;
let lost = false;
// The generation the server last refused, and the ports that already confirmed it.
// A refusal is not an identity: repeating the same confirm from one of these ports
// keeps it, while any other confirm (a port that left and returned, or a fresh port)
// is the reset that lets the same triple reconnect.
let refused: EventsIdentity | undefined;
let refusedPorts = new Set<Port>();

function identity(event: EventHello): EventsIdentity {
	return { installId: event.installId, userId: event.userId, restoreEpoch: event.restoreEpoch };
}

function matches(a: EventsIdentity, b: EventsIdentity): boolean {
	return a.installId === b.installId && a.userId === b.userId && a.restoreEpoch === b.restoreEpoch;
}

// deliver hands one frame to every confirmed port the current hello names. A port
// without a confirmation is never written to, and a port whose confirmation differs
// receives no frame.
function deliver(message: unknown): void {
	const current = hello && identity(hello);
	if (!current) return;
	for (const [port, confirmed] of ports) {
		if (confirmed && matches(confirmed, current)) port.postMessage(message);
	}
}

// noticeHello records a new hello. The first hello of this worker and a hello whose
// identity differs from the previous one tell each confirmed port whose triple differs
// that its identity changed; a reconnect's hello of the same identity tells nobody, so
// stream churn cannot refetch the same projection again.
function noticeHello(event: EventHello): void {
	const previous = hello;
	hello = event;
	const current = identity(event);
	const changed = previous === undefined || !matches(identity(previous), current);
	for (const [port, confirmed] of ports) {
		if (!confirmed) continue;
		if (!matches(confirmed, current)) {
			if (changed) port.postMessage({ type: 'identity-changed' });
		} else if (previous === undefined) port.postMessage({ type: 'hello', hello: event });
	}
}

function confirm(port: Port, confirmed: EventsIdentity): void {
	ports.set(port, confirmed);
	if (lost && refused && matches(confirmed, refused)) {
		// This port already confirmed the refused generation: a repeat is not a new
		// stream. Any other port's confirm is the reset, so clear the refusal.
		if (refusedPorts.has(port)) return;
		refusedPorts.clear();
	}
	lost = false;
	refused = undefined;
	if (hello) {
		// A tab that confirmed after this hello was cached still learns its watermark:
		// the frame is replayed, so it needs no reconnect and no fabricated identity.
		if (matches(confirmed, identity(hello))) port.postMessage({ type: 'hello', hello });
		else {
			port.postMessage({ type: 'identity-changed' });
			// This confirmation names an identity the live stream does not: end it and
			// clear the cached hello, so the reconnect's hello names the current identity
			// instead of the tab staying mute behind a stream it no longer matches.
			hello = undefined;
			stream?.abort();
		}
	}
	start();
}

function start(): void {
	if (stream || lost) return;
	if (![...ports.values()].some(Boolean)) return;
	void run();
}

async function run(): Promise<void> {
	stream = new AbortController();
	let identityLost = false;
	try {
		for await (const frame of eventsStream(stream.signal, lastEventId)) {
			attempt = 0;
			if (frame.id) lastEventId = frame.id;
			if (frame.event === 'hello') {
				noticeHello(frame.data);
			} else {
				deliver({ type: 'frame', frame: frame as EventsFrame });
			}
		}
	} catch (error) {
		const status = error instanceof ApiError ? error.status : 0;
		identityLost = status === 401 || status === 403;
	}
	stream = undefined;
	if (identityLost) {
		lost = true;
		refused = hello ? identity(hello) : [...ports.values()].find(Boolean);
		refusedPorts = new Set();
		if (refused) {
			for (const [port, confirmed] of ports)
				if (confirmed && matches(confirmed, refused)) refusedPorts.add(port);
		}
		hello = undefined;
		for (const port of ports.keys()) port.postMessage({ type: 'identity-lost' });
		return;
	}
	schedule();
}

// A stream that ended or could not be read reconnects after 1, 2, 4 .. <=30 s, and
// never while authority is lost: that is the tab's sign-in, not a retry loop.
function schedule(): void {
	if (lost) return;
	const delay = Math.min(30_000, 1000 * 2 ** attempt);
	attempt = Math.min(attempt + 1, 5);
	setTimeout(start, delay);
}

(globalThis as unknown as { onconnect: (event: MessageEvent) => void }).onconnect = (
	event: MessageEvent
) => {
	const port = event.ports[0] as Port;
	ports.set(port, undefined);
	port.onmessage = (message: MessageEvent) => {
		const data = message.data as { confirm?: EventsIdentity; leave?: boolean; stop?: boolean };
		if (data?.leave) {
			// A tab that left its route (or whose own sign-out attempt failed) drops only
			// its own port: the other tabs keep reading the same stream. A port that
			// returns confirms again and is the reset a refusal waits for.
			ports.delete(port);
			refusedPorts.delete(port);
			if (![...ports.values()].some(Boolean)) stream?.abort();
			return;
		}
		if (data?.stop) {
			// The signing-out tab has no protected reader left. The session it just ended
			// is the one every port shares, so the running stream ends and each tab clears
			// into its own session decision instead of reconnecting.
			lastEventId = undefined;
			hello = undefined;
			lost = true;
			refused = undefined;
			refusedPorts = new Set();
			stream?.abort();
			for (const target of ports.keys()) target.postMessage({ type: 'identity-lost' });
			return;
		}
		if (data?.confirm) confirm(port, data.confirm);
	};
	port.start();
};
