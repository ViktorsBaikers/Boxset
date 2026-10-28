import AxeBuilder from '@axe-core/playwright';
import { expect, test, type Page } from '@playwright/test';

declare const process: {
	env: Record<string, string | undefined>;
	versions: Record<string, string | undefined>;
};

declare global {
	interface Window {
		__boxsetCspReport: (violation: string) => Promise<void>;
		__boxsetSetOnline: (online: boolean) => void;
	}
}

const captureDir = process.env.BOXSET_CAPTURE_DIR;
const challenge = process.env.BOXSET_CAPTURE_CHALLENGE ?? '';
// The public shell a capture run drives is in one lifecycle state: ready for every
// existing shell run, and the recovering state the cold-recovery member
// establishes. The expectation is fixture input, never a value learned from the
// server, so a wrong state still fails.
const expectedCue = process.env.BOXSET_SHELL_EXPECT_STATE ?? 'ready';
const recoveringCue = 'Boxset is recovering. Check the server before trying again.';
// The origins are the full shell URLs, base path included: `dev` serves at the root and `preview`
// is the production shell under its own base (the Go e2e test serves `/media`).
const origins = { dev: process.env.S5_DEV_URL, preview: process.env.S5_PREVIEW_URL };
const viewports = {
	desktop: { width: 1440, height: 900 },
	mobile: { width: 390, height: 844 }
};

const attribution = process.env.BOXSET_CSP_ATTRIBUTION === '1';
const encoder = new TextEncoder();
const recordLimit = 16 * 1024;
let recordBytes = 0;
const enforcingHeader =
	/^default-src 'self'; script-src 'self'(?: 'sha256-[A-Za-z0-9+/]{43}=')*; style-src 'self'; img-src 'self' data: blob:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'$/;

type CspLookupStage = 'describe';
type CspEvent = {
	channel: 'document' | 'audit';
	directive: string;
	reportOnly: boolean;
	blocked: string;
	backendNodeId?: number;
	lookupStage?: CspLookupStage;
	lookupError?: string;
	nodeDescription?: Pick<
		CspNode,
		'backendNodeId' | 'tag' | 'id' | 'ariaLive' | 'ariaAtomic' | 'style'
	> & {
		connected: boolean | null;
		documentToken: string | null;
		documentURL: string | null;
		runtimeContext: string | null;
	};
};
type CspNode = {
	backendNodeId: number;
	tag: string;
	id: string | null;
	ariaLive: string | null;
	ariaAtomic: string | null;
	style: string | null;
	connected: boolean | null;
	documentToken: string | null;
	documentURL: string | null;
	runtimeContext: string | null;
};
type CspDocument = {
	targets: string[];
	targetConfirmed: boolean;
	url: string;
	requests: number;
	navigations: { frame: string; loader: string; url: string }[];
	contexts: { id: string; frame: string }[];
	documents: { token: string; context: string; url: string }[];
	responses: {
		frame: string;
		loader: string;
		url: string;
		header: string;
		reportOnlyHeader: string;
	}[];
	complete: boolean;
	drained: boolean;
	events: CspEvent[];
	nodes: CspNode[];
	failures: string[];
	overflow: boolean;
};
type AttributionCapture = {
	settle: () => Promise<void>;
	finish: (complete?: boolean) => Promise<void>;
};

function attributionOutcome(doc: CspDocument, literal: string) {
	if (
		doc.overflow ||
		doc.events.length > 16 ||
		doc.nodes.length > 4 ||
		doc.nodes.some((node) => encoder.encode(node.style ?? '').length > 1024)
	) {
		return 'unproven-overflow';
	}
	if (doc.nodes.some((node) => node.style === null)) return 'unproven';
	if (doc.nodes.length > 1 || doc.nodes.some((node) => node.style !== literal)) {
		return 'owner-answer-required';
	}
	if (
		doc.failures.length ||
		doc.targets.length !== 1 ||
		!doc.targets[0] ||
		!doc.targetConfirmed ||
		doc.requests !== 1 ||
		doc.navigations.length !== 1 ||
		doc.contexts.length !== 1 ||
		doc.documents.length !== 1 ||
		doc.responses.length !== 1 ||
		!doc.complete ||
		!doc.drained ||
		doc.nodes.length !== 1 ||
		!doc.events.some((event) => event.channel === 'document') ||
		!doc.events.some((event) => event.channel === 'audit') ||
		doc.events.filter((event) => event.channel === 'document').length >
			doc.events.filter((event) => event.channel === 'audit').length ||
		doc.events.some(
			(event) =>
				event.directive !== 'style-src-attr' ||
				event.reportOnly ||
				event.blocked !== 'inline' ||
				event.lookupError ||
				(event.channel === 'audit' &&
					(!event.backendNodeId ||
						!doc.nodes.some((node) => node.backendNodeId === event.backendNodeId)))
		)
	) {
		return 'unproven';
	}
	const navigation = doc.navigations[0];
	const context = doc.contexts[0];
	const identity = doc.documents[0];
	const response = doc.responses[0];
	const responseURL = new URL(doc.url);
	responseURL.hash = '';
	if (
		!navigation.frame ||
		!navigation.loader ||
		navigation.url !== doc.url ||
		!context.id ||
		context.frame !== navigation.frame ||
		!identity.token ||
		identity.context !== context.id ||
		identity.url !== doc.url ||
		response.frame !== navigation.frame ||
		response.loader !== navigation.loader ||
		response.url !== responseURL.href ||
		!enforcingHeader.test(response.header) ||
		response.reportOnlyHeader
	)
		return 'unproven';
	const node = doc.nodes[0];
	return node.tag === 'DIV' &&
		node.id === 'svelte-announcer' &&
		node.ariaLive === 'assertive' &&
		node.ariaAtomic === 'true' &&
		node.style === literal
		? 'attributed'
		: 'unproven';
}

function emitAttribution(record: object) {
	const line = `CSP-ATTRIBUTION ${JSON.stringify(record)}`;
	const bytes = encoder.encode(line + '\n').length;
	// Reserve room for the explicit overflow record; never cut a decisive attribute.
	if (recordBytes + bytes > recordLimit - 128) {
		const overflow =
			'CSP-ATTRIBUTION {"outcome":"unproven-overflow","reason":"global-record-limit"}';
		if (recordBytes + encoder.encode(overflow + '\n').length <= recordLimit) {
			console.log(overflow);
			recordBytes += encoder.encode(overflow + '\n').length;
		}
		throw new Error('CSP-ATTRIBUTION global-record-limit');
	}
	recordBytes += bytes;
	console.log(line);
}

async function attributeDigest(style: string) {
	const bytes = encoder.encode(style);
	const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', bytes));
	return {
		utf8Base64: btoa(String.fromCharCode(...bytes)),
		sha256Hex: [...digest].map((byte) => byte.toString(16).padStart(2, '0')).join(''),
		sha256Base64: btoa(String.fromCharCode(...digest))
	};
}

async function captureAttribution(page: Page, name: string, url: string, deadline: number) {
	const literal = process.env.BOXSET_CSP_SOURCE_STYLE ?? '';
	const sourceHash = process.env.BOXSET_CSP_SOURCE_SHA256 ?? '';
	const doc: CspDocument = {
		targets: [],
		targetConfirmed: false,
		url,
		requests: 0,
		navigations: [],
		contexts: [],
		documents: [],
		responses: [],
		complete: false,
		drained: false,
		events: [],
		nodes: [],
		failures: [],
		overflow: false
	};
	const pending = new Set<Promise<void>>();
	const contexts = new Map<number, string>();
	const cdp = await page
		.context()
		.newCDPSession(page)
		.catch((error: unknown) => {
			emitAttribution({ name, outcome: 'unproven', ...doc, failures: ['cdp-session-unavailable'] });
			throw error;
		});
	let target = '';
	let browserContext = '';
	let armed = false;
	let emitted = false;
	let finishPromise: Promise<void> | undefined;
	const fail = (reason: string) => {
		if (!doc.failures.includes(reason) && doc.failures.length < 16) doc.failures.push(reason);
		else if (!doc.failures.includes(reason)) doc.overflow = true;
	};

	cdp.on('Page.frameAttached', () => fail('unexpected-subframe'));
	cdp.on('Page.frameNavigated', ({ frame }) => {
		if (!armed && frame.url === 'about:blank') return;
		if (frame.parentId) fail('unexpected-subframe');
		if (doc.navigations.length === 16) doc.overflow = true;
		else
			doc.navigations.push({
				frame: frame.id,
				loader: frame.loaderId,
				url: frame.url + (frame.urlFragment ?? '')
			});
	});
	cdp.on('Network.requestWillBeSent', ({ type, redirectResponse }) => {
		if (type !== 'Document') return;
		doc.requests++;
		if (redirectResponse) fail('document-redirect');
	});
	cdp.on('Network.responseReceived', ({ type, frameId, loaderId, response }) => {
		if (type !== 'Document') return;
		if (doc.responses.length === 16) {
			doc.overflow = true;
			return;
		}
		const header = (name: string) =>
			String(
				Object.entries(response.headers).find(([key]) => key.toLowerCase() === name)?.[1] ?? ''
			);
		const enforcing = header('content-security-policy');
		const reportOnly = header('content-security-policy-report-only');
		if (encoder.encode(enforcing + reportOnly + response.url).length > 2048) {
			doc.overflow = true;
			fail('response-byte-overflow');
			return;
		}
		doc.responses.push({
			frame: frameId ?? '',
			loader: loaderId,
			url: response.url,
			header: enforcing,
			reportOnlyHeader: reportOnly
		});
	});
	cdp.on('Runtime.executionContextCreated', ({ context }) => {
		if (context.auxData?.type !== 'default') return;
		if (!armed && !doc.navigations.length) return;
		if (doc.contexts.length === 16) {
			doc.overflow = true;
			return;
		}
		contexts.set(context.id, context.uniqueId);
		doc.contexts.push({ id: context.uniqueId, frame: context.auxData.frameId ?? '' });
	});
	cdp.on('Runtime.bindingCalled', ({ name: binding, payload, executionContextId }) => {
		if (binding !== '__boxsetCspRecord') return;
		if (encoder.encode(payload).length > 4096) {
			doc.overflow = true;
			fail('event-byte-overflow');
			return;
		}
		let message: {
			kind: string;
			token: string;
			url: string;
			directive: string;
			reportOnly: boolean;
			blocked: string;
		};
		try {
			message = JSON.parse(payload);
		} catch {
			fail('invalid-event-payload');
			return;
		}
		if (
			!message ||
			typeof message.token !== 'string' ||
			typeof message.url !== 'string' ||
			!['document', 'violation', 'overflow'].includes(message.kind) ||
			(message.kind === 'violation' &&
				(typeof message.directive !== 'string' ||
					typeof message.reportOnly !== 'boolean' ||
					typeof message.blocked !== 'string'))
		) {
			fail('invalid-event-payload');
			return;
		}
		const context = contexts.get(executionContextId) ?? '';
		if (message.kind === 'document') {
			if (doc.documents.length === 16) doc.overflow = true;
			else doc.documents.push({ token: message.token, context, url: message.url });
			return;
		}
		if (
			doc.documents.length !== 1 ||
			doc.documents[0].context !== context ||
			doc.documents[0].token !== message.token ||
			message.url !== url
		)
			fail('event-document-ambiguous');
		if (message.kind === 'overflow' || doc.events.length === 16) doc.overflow = true;
		else
			doc.events.push({
				channel: 'document',
				directive: message.directive,
				reportOnly: message.reportOnly,
				blocked: message.blocked
			});
	});
	cdp.on('Target.targetCreated', ({ targetInfo }) => {
		if (!armed && targetInfo.browserContextId !== browserContext) return;
		if (!doc.targets.includes(targetInfo.targetId)) {
			if (doc.targets.length === 16) doc.overflow = true;
			else doc.targets.push(targetInfo.targetId);
		}
		if (targetInfo.targetId !== target || targetInfo.type !== 'page') fail('unexpected-target');
	});
	cdp.on('Target.targetInfoChanged', ({ targetInfo }) => {
		if (targetInfo.targetId !== target) return;
		if (targetInfo.browserContextId !== browserContext || targetInfo.type !== 'page')
			fail('changed-target-identity');
		if (targetInfo.url !== 'about:blank' && targetInfo.url !== url) fail('unexpected-target-url');
	});
	cdp.on('Audits.issueAdded', ({ issue }) => {
		const details = issue.details.contentSecurityPolicyIssueDetails;
		if (!details) return;
		if (!armed || !doc.navigations.length) fail('pre-navigation-csp-issue');
		if (doc.events.length === 16) {
			doc.overflow = true;
			return;
		}
		const event: CspEvent = {
			channel: 'audit',
			directive: details.violatedDirective,
			reportOnly: details.isReportOnly,
			blocked:
				details.contentSecurityPolicyViolationType === 'kInlineViolation'
					? 'inline'
					: (details.blockedURL ?? details.contentSecurityPolicyViolationType),
			backendNodeId: details.violatingNodeId
		};
		doc.events.push(event);
		if (!details.violatingNodeId) {
			fail('absent-node-id');
			return;
		}
		const backendNodeId = details.violatingNodeId;
		const proof = (async () => {
			// Only the backend identity delivered by Audits is described.
			const { node } = await cdp.send('DOM.describeNode', { backendNodeId, depth: 0 });
			if (node.backendNodeId !== backendNodeId) {
				fail('node-description-identity-mismatch');
				return;
			}
			const attributes = new Map<string, string>();
			for (let i = 0; i < (node.attributes?.length ?? 0); i += 2) {
				attributes.set(node.attributes![i], node.attributes![i + 1]);
			}
			const style = attributes.get('style') ?? null;
			if (style !== null && encoder.encode(style).length > 1024) {
				doc.overflow = true;
				fail('attribute-byte-overflow');
				return;
			}
			const description: CspNode = {
				backendNodeId,
				tag: node.nodeName,
				id: attributes.get('id') ?? null,
				ariaLive: attributes.get('aria-live') ?? null,
				ariaAtomic: attributes.get('aria-atomic') ?? null,
				style,
				connected: null,
				documentToken: null,
				documentURL: null,
				runtimeContext: null
			};
			const prior = doc.nodes.find((other) => other.backendNodeId === backendNodeId);
			if (prior && JSON.stringify(prior) !== JSON.stringify(description)) {
				event.nodeDescription = description;
				fail('node-description-conflict');
			}
			if (!prior) {
				if (doc.nodes.length === 4) doc.overflow = true;
				else doc.nodes.push(description);
			}
		})().catch((error: unknown) => {
			const message = error instanceof Error ? error.message : 'protocol-error-unavailable';
			event.lookupStage = 'describe';
			event.lookupError =
				encoder.encode(message).length <= 256 ? message : 'protocol-error-byte-overflow';
			fail('node-describe-unavailable');
		});
		pending.add(proof);
		void proof.finally(() => pending.delete(proof));
	});
	cdp.on('close', () => {
		if (!emitted) fail('cdp-disconnected');
	});

	const settle = async () => {
		await cdp.send('Runtime.evaluate', {
			expression:
				'(() => { const { promise, resolve } = Promise.withResolvers(); setTimeout(resolve, 0); return promise; })()',
			awaitPromise: true
		});
		while (pending.size) await Promise.all(pending);
	};
	const finish = (complete = false) => {
		if (finishPromise) return finishPromise;
		finishPromise = (async () => {
			doc.complete = complete;
			if (!complete) fail('product-flow-incomplete');
			const expiry = Promise.withResolvers<never>();
			const timer = setTimeout(
				() => expiry.reject(new Error('measurement-deadline-expired')),
				Math.max(0, deadline - Date.now())
			);
			try {
				await Promise.race([
					(async () => {
						await settle();
						// The disable acknowledgement fences the audit stream, not a quiet interval.
						await cdp.send('Audits.disable');
						while (pending.size) await Promise.all(pending);
						const snapshot = await cdp.send('Runtime.evaluate', {
							expression: 'globalThis.__boxsetCspSnapshot()',
							returnByValue: true
						});
						const actual = snapshot.result.value as
							{ token: string; url: string; count: number } | undefined;
						if (
							snapshot.exceptionDetails ||
							!actual ||
							doc.documents.length !== 1 ||
							actual.token !== doc.documents[0].token ||
							actual.url !== url ||
							actual.count !== doc.events.filter((event) => event.channel === 'document').length
						)
							fail('document-drain-unmatched');
						const { targetInfo } = await cdp.send('Target.getTargetInfo');
						doc.targetConfirmed =
							targetInfo.targetId === target &&
							targetInfo.browserContextId === browserContext &&
							targetInfo.type === 'page' &&
							targetInfo.url === url;
						while (pending.size) await Promise.all(pending);
						doc.drained = true;
					})(),
					expiry.promise
				]);
			} catch (error) {
				fail(
					error instanceof Error && error.message === 'measurement-deadline-expired'
						? error.message
						: 'measurement-barrier-unavailable'
				);
			} finally {
				clearTimeout(timer);
			}
			if (!literal || encoder.encode(literal).length > 1024 || !/^[0-9a-f]{64}$/.test(sourceHash))
				fail('installed-source-unproven');
			try {
				const nodes = await Promise.all(
					doc.nodes.map(async (node) => {
						const digest = node.style === null ? null : await attributeDigest(node.style);
						return {
							...node,
							...(digest && {
								sha256Hex: digest.sha256Hex,
								sha256Base64: digest.sha256Base64
							})
						};
					})
				);
				const outcome = attributionOutcome(doc, literal);
				// Equal bindings share their observed values only after validation succeeds.
				const provenance =
					outcome === 'attributed'
						? {
								target: doc.targets[0],
								frame: doc.navigations[0].frame,
								loader: doc.navigations[0].loader,
								context: doc.contexts[0].id,
								token: doc.documents[0].token,
								url: doc.documents[0].url,
								responseURL: doc.responses[0].url,
								header: doc.responses[0].header,
								reportOnlyHeader: doc.responses[0].reportOnlyHeader,
								isolation: {
									targets: doc.targets.length,
									navigations: doc.navigations.length,
									requests: doc.requests,
									contexts: doc.contexts.length,
									documents: doc.documents.length,
									targetConfirmed: doc.targetConfirmed,
									complete: doc.complete,
									drained: doc.drained
								},
								events: doc.events
							}
						: doc;
				emitAttribution({
					name,
					outcome,
					...provenance,
					nodes,
					source: {
						sha256Hex: sourceHash,
						literal
					}
				});
				emitted = true;
				if (complete) expect(outcome, 'CSP attribution only, not acceptance').toBe('attributed');
			} finally {
				await cdp.detach();
			}
		})();
		return finishPromise;
	};
	try {
		const { targetInfo } = await cdp.send('Target.getTargetInfo');
		target = targetInfo.targetId;
		browserContext = targetInfo.browserContextId ?? '';
		doc.targets.push(target);
		if (!browserContext || targetInfo.type !== 'page' || targetInfo.url !== 'about:blank')
			fail('target-not-fresh');
		await cdp.send('Target.setDiscoverTargets', { discover: true });
		await cdp.send('Page.enable');
		await cdp.send('Network.enable');
		await cdp.send('Runtime.enable');
		await cdp.send('Runtime.addBinding', { name: '__boxsetCspRecord' });
		await cdp.send('Page.addScriptToEvaluateOnNewDocument', {
			source: `(() => {
				const token = crypto.randomUUID();
				let count = 0;
				globalThis.__boxsetCspSnapshot = () => ({ token, url: document.URL, count });
				const emit = (event) => globalThis.__boxsetCspRecord(JSON.stringify({
					token, url: document.URL, ...event
				}));
				document.addEventListener('securitypolicyviolation', (event) => {
					if (++count <= 16) emit({ kind: 'violation', directive: event.effectiveDirective,
						reportOnly: event.disposition === 'report', blocked: event.blockedURI });
					else if (count === 17) emit({ kind: 'overflow' });
				});
				emit({ kind: 'document' });
			})()`
		});
		await cdp.send('Audits.enable');
		armed = true;
	} catch (error) {
		fail('unsupported-cdp-capability');
		await finish();
		throw error;
	}
	return { settle, finish };
}

test.describe('CSP attribution fixtures', () => {
	const literal = 'position: absolute';
	const fixture = (): CspDocument => ({
		targets: ['target-1'],
		targetConfirmed: true,
		url: 'http://127.0.0.1/',
		requests: 1,
		navigations: [{ frame: 'frame-1', loader: 'loader-1', url: 'http://127.0.0.1/' }],
		contexts: [{ id: 'context-1', frame: 'frame-1' }],
		documents: [{ token: 'document-1', context: 'context-1', url: 'http://127.0.0.1/' }],
		responses: [
			{
				frame: 'frame-1',
				loader: 'loader-1',
				url: 'http://127.0.0.1/',
				header:
					"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'",
				reportOnlyHeader: ''
			}
		],
		complete: true,
		drained: true,
		events: [
			{ channel: 'document', directive: 'style-src-attr', reportOnly: false, blocked: 'inline' },
			{
				channel: 'audit',
				directive: 'style-src-attr',
				reportOnly: false,
				blocked: 'inline',
				backendNodeId: 7
			}
		],
		nodes: [
			{
				backendNodeId: 7,
				tag: 'DIV',
				id: 'svelte-announcer',
				ariaLive: 'assertive',
				ariaAtomic: 'true',
				style: literal,
				connected: null,
				documentToken: null,
				documentURL: null,
				runtimeContext: null
			}
		],
		failures: [],
		overflow: false
	});
	test('matching bytes and duplicate event deliveries', () => {
		const doc = fixture();
		doc.events.push({ ...doc.events[0] }, { ...doc.events[1] });
		expect(attributionOutcome(doc, literal)).toBe('attributed');
		doc.events.push({ ...doc.events[1] });
		expect(attributionOutcome(doc, literal), 'duplicate audit delivery').toBe('attributed');
	});
	test('absent node ID', () => {
		const doc = fixture();
		delete doc.events[1].backendNodeId;
		expect(attributionOutcome(doc, literal)).toBe('unproven');
	});
	test('different bytes', () => {
		const doc = fixture();
		doc.nodes[0].style += ' ';
		expect(attributionOutcome(doc, literal)).toBe('owner-answer-required');
	});
	test('two nodes with equal bytes and duplicate DOM id', () => {
		const doc = fixture();
		doc.nodes.push({ ...doc.nodes[0], backendNodeId: 8 });
		expect(attributionOutcome(doc, literal)).toBe('owner-answer-required');
	});
	test('evidence overflow', () => {
		const doc = fixture();
		doc.events.push(...Array.from({ length: 7 }, () => doc.events.slice(0, 2)).flat());
		expect(attributionOutcome(doc, literal), '16 events').toBe('attributed');
		doc.events.push({ ...doc.events[0] });
		expect(attributionOutcome(doc, literal), '17 events').toBe('unproven-overflow');
		const bytes = fixture();
		const unicode = 'é'.repeat(512);
		bytes.nodes[0].style = unicode;
		expect(attributionOutcome(bytes, unicode), '1024 UTF-8 bytes').toBe('attributed');
		bytes.nodes[0].style += ' ';
		expect(attributionOutcome(bytes, unicode), '1025 UTF-8 bytes').toBe('unproven-overflow');
		const nodes = fixture();
		nodes.nodes.push(...[8, 9, 10].map((backendNodeId) => ({ ...nodes.nodes[0], backendNodeId })));
		expect(attributionOutcome(nodes, literal), 'four nodes').toBe('owner-answer-required');
		nodes.nodes.push({ ...nodes.nodes[0], backendNodeId: 11 });
		expect(attributionOutcome(nodes, literal), 'five nodes').toBe('unproven-overflow');
	});
	test('unmeasured node affiliation, missing attribute and ambiguous document', () => {
		const unmeasured = fixture();
		expect(unmeasured.nodes[0].connected).toBeNull();
		expect(unmeasured.nodes[0].documentToken).toBeNull();
		expect(unmeasured.nodes[0].documentURL).toBeNull();
		expect(unmeasured.nodes[0].runtimeContext).toBeNull();
		expect(attributionOutcome(unmeasured, literal)).toBe('attributed');
		const missing = fixture();
		missing.nodes[0].style = null;
		expect(attributionOutcome(missing, literal)).toBe('unproven');
		const ambiguous = fixture();
		ambiguous.documents[0].context = 'another-context';
		expect(attributionOutcome(ambiguous, literal)).toBe('unproven');
		const missingAudit = fixture();
		missingAudit.events.push({ ...missingAudit.events[0] });
		expect(attributionOutcome(missingAudit, literal), 'unattributed document event').toBe(
			'unproven'
		);
	});
	test('other directives and report-only evidence cannot match', () => {
		const other = fixture();
		other.events.push({ ...other.events[0], directive: 'script-src-elem' });
		expect(attributionOutcome(other, literal)).toBe('unproven');
		const reportOnly = fixture();
		reportOnly.events[1].reportOnly = true;
		expect(attributionOutcome(reportOnly, literal)).toBe('unproven');
		const differentHeader = fixture();
		differentHeader.responses[0].header += "; style-src-attr 'unsafe-hashes'";
		expect(attributionOutcome(differentHeader, literal)).toBe('unproven');
	});
	const invalid: [string, (doc: CspDocument) => void][] = [
		[
			'wrong tag',
			(doc) => {
				doc.nodes[0].tag = 'SPAN';
			}
		],
		[
			'missing announcer id',
			(doc) => {
				doc.nodes[0].id = null;
			}
		],
		[
			'missing live announcement',
			(doc) => {
				doc.nodes[0].ariaLive = null;
			}
		],
		[
			'missing atomic announcement',
			(doc) => {
				doc.nodes[0].ariaAtomic = null;
			}
		],
		[
			'missing header',
			(doc) => {
				doc.responses[0].header = '';
			}
		],
		[
			'missing response',
			(doc) => {
				doc.responses = [];
			}
		],
		[
			'report-only header',
			(doc) => {
				doc.responses[0].reportOnlyHeader = "style-src 'self'";
			}
		],
		[
			'mismatched response URL',
			(doc) => {
				doc.responses[0].url += 'other';
			}
		],
		[
			'missing loader',
			(doc) => {
				doc.navigations[0].loader = '';
			}
		],
		[
			'mismatched loader',
			(doc) => {
				doc.responses[0].loader = 'other-loader';
			}
		],
		[
			'missing context',
			(doc) => {
				doc.contexts = [];
			}
		],
		[
			'mismatched context frame',
			(doc) => {
				doc.contexts[0].frame = 'other-frame';
			}
		],
		[
			'mismatched document URL',
			(doc) => {
				doc.documents[0].url += 'other';
			}
		],
		[
			'missing token',
			(doc) => {
				doc.documents[0].token = '';
			}
		],
		[
			'second navigation',
			(doc) => {
				doc.navigations.push({ ...doc.navigations[0], loader: 'loader-2' });
			}
		],
		[
			'redirect',
			(doc) => {
				doc.requests++;
			}
		],
		[
			'unexpected child document',
			(doc) => {
				doc.contexts.push({ id: 'child-context', frame: 'child-frame' });
			}
		],
		[
			'unexpected target',
			(doc) => {
				doc.targets.push('worker-target');
			}
		],
		[
			'changed target',
			(doc) => {
				doc.targetConfirmed = false;
			}
		],
		[
			'pre-navigation issue',
			(doc) => {
				doc.failures.push('pre-navigation-csp-issue');
			}
		],
		[
			'failed description',
			(doc) => {
				doc.nodes = [];
				doc.events[1].lookupError = 'No node with given id found';
			}
		],
		[
			'conflicting description',
			(doc) => {
				doc.failures.push('node-description-conflict');
			}
		],
		[
			'incomplete product flow',
			(doc) => {
				doc.complete = false;
			}
		],
		[
			'incomplete final drain',
			(doc) => {
				doc.drained = false;
			}
		]
	];
	for (const [label, mutate] of invalid) {
		test(label, () => {
			const doc = fixture();
			mutate(doc);
			expect(attributionOutcome(doc, literal)).toBe('unproven');
		});
	}
	test('provenance collision mutant rejects equal bytes in one target', async () => {
		const first = fixture();
		const second = fixture();
		second.documents[0].token = 'document-2';
		expect(await attributeDigest(first.nodes[0].style!)).toEqual(
			await attributeDigest(second.nodes[0].style!)
		);
		const collision = { ...first, documents: [...first.documents, ...second.documents] };
		expect(attributionOutcome(collision, literal)).toBe('unproven');
		second.targets = ['target-2'];
		expect(first.nodes[0].backendNodeId).toBe(second.nodes[0].backendNodeId);
		expect(attributionOutcome(first, literal)).toBe('attributed');
		expect(attributionOutcome(second, literal)).toBe('attributed');
		expect(
			new Set([first, second].map((doc) => `${doc.targets[0]}:${doc.nodes[0].backendNodeId}`)).size
		).toBe(2);
	});
	test('final drain includes a late second offender before reevaluating history', async () => {
		const doc = fixture();
		doc.drained = false;
		expect(attributionOutcome(doc, literal)).toBe('unproven');
		const arrival = Promise.withResolvers<void>();
		const description = arrival.promise.then(() => {
			doc.events.push({ ...doc.events[1], backendNodeId: 8 });
			doc.nodes.push({ ...doc.nodes[0], backendNodeId: 8 });
		});
		arrival.resolve();
		await description;
		doc.drained = true;
		expect(doc.events).toHaveLength(3);
		expect(attributionOutcome(doc, literal)).toBe('owner-answer-required');
	});
	test('hashes retain exact UTF-8 bytes', async () => {
		expect(await attributeDigest('abc')).toEqual({
			utf8Base64: 'YWJj',
			sha256Hex: 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad',
			sha256Base64: 'ungWv48Bz+pBQUDeXa4iI7ADYaOWF3qctBD/YfIAFa0='
		});
		expect((await attributeDigest('abc ')).sha256Hex).not.toBe(
			(await attributeDigest('abc')).sha256Hex
		);
	});
	test('global record overflow emits an explicit bounded failure', () => {
		const originalLog = console.log;
		const originalBytes = recordBytes;
		const lines: string[] = [];
		try {
			console.log = (line: unknown) => {
				lines.push(String(line));
			};
			recordBytes = recordLimit - 128;
			expect(() => emitAttribution({ attribute: 'x'.repeat(129) })).toThrow(
				'CSP-ATTRIBUTION global-record-limit'
			);
			expect(lines).toEqual([
				'CSP-ATTRIBUTION {"outcome":"unproven-overflow","reason":"global-record-limit"}'
			]);
			expect(recordBytes).toBeLessThanOrEqual(recordLimit);
		} finally {
			console.log = originalLog;
			recordBytes = originalBytes;
		}
	});
	test('isolated template-generated style issue under strict CSP', async ({
		browser
	}, testInfo) => {
		const context = await browser.newContext();
		const page = await context.newPage();
		const url = 'http://127.0.0.1/template-fixture';
		const markup = `<div id="svelte-announcer" aria-live="assertive" aria-atomic="true" style="${literal}"></div>`;
		const script = `const template = document.createElement('template');
			template.innerHTML = ${JSON.stringify(markup)};
			document.body.append(document.importNode(template.content, true));`;
		const originalLog = console.log;
		const originalBytes = recordBytes;
		const originalStyle = process.env.BOXSET_CSP_SOURCE_STYLE;
		const originalHash = process.env.BOXSET_CSP_SOURCE_SHA256;
		const lines: string[] = [];
		let diagnostic: AttributionCapture | undefined;
		try {
			// Synthetic records never enter the eight-document product transcript.
			console.log = (line: unknown) => {
				lines.push(String(line));
			};
			process.env.BOXSET_CSP_SOURCE_STYLE = literal;
			process.env.BOXSET_CSP_SOURCE_SHA256 = (await attributeDigest(script)).sha256Hex;
			await page.route('http://127.0.0.1/**', async (route) => {
				const javascript = route.request().url().endsWith('/template.js');
				await route.fulfill({
					status: 200,
					contentType: javascript ? 'application/javascript' : 'text/html',
					headers: { 'content-security-policy': fixture().responses[0].header },
					body: javascript
						? script
						: '<!doctype html><html><body><script src="/template.js"></script></body></html>'
				});
			});
			diagnostic = await captureAttribution(
				page,
				'template-fixture',
				url,
				Date.now() + testInfo.timeout
			);
			await page.goto(url);
			await diagnostic.finish(true);
			expect(lines, 'one synthetic record, no product acquisition').toHaveLength(1);
			const record = JSON.parse(lines[0].slice('CSP-ATTRIBUTION '.length)) as {
				outcome: string;
				nodes: CspNode[];
				header: string;
				isolation: { targets: number; documents: number; drained: boolean };
			};
			expect(record.outcome, lines[0]).toBe('attributed');
			expect(record.isolation.targets).toBe(1);
			expect(record.isolation.documents).toBe(1);
			expect(record.nodes).toHaveLength(1);
			expect(record.nodes[0]).toMatchObject({
				tag: 'DIV',
				id: 'svelte-announcer',
				ariaLive: 'assertive',
				ariaAtomic: 'true',
				style: literal,
				connected: null,
				documentToken: null,
				documentURL: null,
				runtimeContext: null
			});
			expect(record.nodes[0].backendNodeId).toBeGreaterThan(0);
			expect(record.header).toBe(fixture().responses[0].header);
			expect(record.isolation.drained).toBe(true);
		} finally {
			try {
				await diagnostic?.finish();
			} finally {
				console.log = originalLog;
				recordBytes = originalBytes;
				if (originalStyle === undefined) delete process.env.BOXSET_CSP_SOURCE_STYLE;
				else process.env.BOXSET_CSP_SOURCE_STYLE = originalStyle;
				if (originalHash === undefined) delete process.env.BOXSET_CSP_SOURCE_SHA256;
				else process.env.BOXSET_CSP_SOURCE_SHA256 = originalHash;
				await context.close();
			}
		}
	});
});
function isSessionURL(origin: string, location: string) {
	return location === new URL('api/v1/session', origin).href;
}

function expectedShellConsoleError(
	host: string | undefined,
	origin: string,
	text: string,
	location: string
) {
	if (host === 'static')
		return (
			location === new URL('ping', origin).href &&
			text === 'Failed to load resource: the server responded with a status of 404 (Not Found)'
		);
	return (
		host === 'production' &&
		isSessionURL(origin, location) &&
		text === 'Failed to load resource: the server responded with a status of 401 (Unauthorized)'
	);
}

test('runs in a Bun worker', () => {
	expect(process.versions.bun).toBeTruthy();
	const missing = 'Failed to load resource: the server responded with a status of 404 (Not Found)';
	const denied =
		'Failed to load resource: the server responded with a status of 401 (Unauthorized)';
	for (const origin of ['http://127.0.0.1:5173/', 'http://127.0.0.1:4173/__boxset_base__/']) {
		const session = new URL('api/v1/session', origin).href;
		expect(isSessionURL(origin, session)).toBe(true);
		expect(expectedShellConsoleError('production', origin, denied, session)).toBe(true);
		for (const location of [
			new URL('api/v1/unexpected/api/v1/session', origin).href,
			new URL('/wrong-base/api/v1/session', origin).href,
			new URL('api/v1/session?unexpected=1', origin).href,
			new URL('_app/missing.js', origin).href,
			new URL('sign-in', origin).href,
			session.replace('127.0.0.1', 'localhost'),
			session.replace('http:', 'https:'),
			session.replace(':5173', ':5174').replace(':4173', ':4174'),
			''
		]) {
			expect(isSessionURL(origin, location)).toBe(false);
			expect(expectedShellConsoleError('production', origin, denied, location)).toBe(false);
		}
		expect(expectedShellConsoleError('static', origin, denied, session)).toBe(false);
		expect(expectedShellConsoleError(undefined, origin, denied, session)).toBe(false);
		expect(
			expectedShellConsoleError('production', origin, 'unexpected console error', session)
		).toBe(false);
		expect(expectedShellConsoleError('production', origin, missing, session)).toBe(false);
		const ping = new URL('ping', origin).href;
		expect(expectedShellConsoleError('static', origin, missing, ping)).toBe(true);
		for (const location of [
			new URL('nested/ping', origin).href,
			new URL('ping?unexpected=1', origin).href,
			new URL('_app/missing.js', origin).href,
			new URL('sign-in', origin).href,
			new URL('/wrong-base/ping', origin).href,
			ping.replace('127.0.0.1', 'localhost'),
			''
		]) {
			expect(expectedShellConsoleError('static', origin, missing, location)).toBe(false);
		}
		expect(expectedShellConsoleError('production', origin, missing, ping)).toBe(false);
		expect(expectedShellConsoleError(undefined, origin, missing, ping)).toBe(false);
		expect(expectedShellConsoleError('static', origin, 'unexpected console error', ping)).toBe(
			false
		);
		expect(
			expectedShellConsoleError(
				'static',
				origin,
				missing.replace('404 (Not Found)', '500 (Internal Server Error)'),
				ping
			)
		).toBe(false);
	}
});

for (const mode of ['dev', 'preview'] as const) {
	for (const view of ['desktop', 'mobile'] as const) {
		const name = `${mode}-${view}`;
		const size = viewports[view];

		test.describe(name, () => {
			test.use({ viewport: size, deviceScaleFactor: 1 });

			test('loads the public shell, offers Retry offline, passes axe and captures', async ({
				page: ordinaryPage,
				browser
			}, testInfo) => {
				expect(captureDir, 'BOXSET_CAPTURE_DIR').toBeTruthy();
				expect(challenge, 'BOXSET_CAPTURE_CHALLENGE').toMatch(/^[0-9a-f]{16}$/);
				expect(['ready', 'recovering'], 'fixture cue state').toContain(expectedCue);
				const origin = origins[mode];
				expect(origin, `origin of ${mode}`).toBeTruthy();
				const host = process.env.BOXSET_SHELL_HOST;
				expect(['production', 'static'], 'explicit shell host designation').toContain(host);

				const deadline = Date.now() + testInfo.timeout;
				if (attribution) await ordinaryPage.context().close();
				const documentURLs = attribution
					? [origin!, new URL('?deep=link#section', origin).href]
					: [origin!];
				for (const [index, documentURL] of documentURLs.entries()) {
					const context = attribution
						? await browser.newContext({ viewport: size, deviceScaleFactor: 1 })
						: ordinaryPage.context();
					let diagnostic: AttributionCapture | undefined;
					const productViolations: string[] = [];
					try {
						const page = attribution ? await context.newPage() : ordinaryPage;
						await page.exposeBinding('__boxsetCspReport', (_source, violation: string) => {
							if (productViolations.length < 16) productViolations.push(violation);
							else if (productViolations.length === 16) productViolations.push('event-overflow');
						});
						if (attribution)
							diagnostic = await captureAttribution(
								page,
								`${name}-${index === 0 ? 'initial' : 'deep-link'}`,
								documentURL,
								deadline
							);
						const problems: string[] = [];
						page.on('console', (message) => {
							if (
								message.type() === 'error' &&
								!expectedShellConsoleError(host, origin!, message.text(), message.location().url)
							)
								problems.push(message.text());
						});
						page.on('pageerror', (error) => problems.push(error.message));
						const requests: Array<{ path: string; session: boolean }> = [];
						page.on('request', (request) =>
							requests.push({
								path: new URL(request.url()).pathname,
								session: isSessionURL(origin!, request.url())
							})
						);
						const cues: Array<{ state?: string; cache?: string }> = [];
						const sessions: number[] = [];
						page.on('response', (response) => {
							const path = new URL(response.url()).pathname;
							if (path.endsWith('/ping'))
								cues.push({
									state: response.headers()['x-boxset-state'],
									cache: response.headers()['cache-control']
								});
							if (isSessionURL(origin!, response.url())) sessions.push(response.status());
						});
						const settled = async () => {
							await expect(page.getByRole('status')).toHaveText(
								expectedCue === 'ready'
									? host === 'production'
										? 'Sign in to check this Boxset.'
										: 'Boxset cannot be reached from this device. Check the connection, then try again.'
									: recoveringCue
							);
							if (expectedCue === 'ready') {
								await expect(
									page.getByRole('link', { name: 'Sign in', exact: true })
								).toHaveAttribute('href', new URL('sign-in', origin!).pathname);
							}
							expect(await page.locator('form, input').count()).toBe(0);
							expect(
								requests.filter((request) => request.path.includes('/api/') && !request.session)
							).toEqual([]);
							expect(cues.length).toBeGreaterThan(0);
							if (host === 'production') {
								expect(
									cues.every((cue) => cue.state === expectedCue && cue.cache === 'no-store')
								).toBe(true);
								if (expectedCue === 'ready') {
									expect(sessions.length).toBeGreaterThan(0);
									expect(sessions.every((status) => status === 401)).toBe(true);
								} else {
									// A non-ready cue paints the public state before any session
									// query, so nothing protected is requested at all.
									expect(sessions).toEqual([]);
								}
							} else {
								expect(cues.every((cue) => cue.state === undefined)).toBe(true);
								expect(sessions).toEqual([]);
								await expect(page.getByRole('button', { name: 'Retry' })).toBeVisible();
							}
						};
						let release!: () => void;
						const hold = new Promise<void>((resolve) => {
							release = resolve;
						});
						const timer = setTimeout(() => release(), 10000);
						await page.route('**/ping', async (route) => {
							await hold;
							await route.continue();
						});
						// The page's only input is the browser's connectivity. The runner child has just
						// loopback, which Chromium reports as offline, so the spec states the connectivity
						// each step needs (online first) instead of inheriting the host's.
						await page.addInitScript((diagnostic) => {
							if (!diagnostic)
								document.addEventListener('securitypolicyviolation', (event) => {
									void window.__boxsetCspReport(
										`${document.URL} ${event.violatedDirective} ${event.blockedURI}`
									);
								});
							let online = true;
							Object.defineProperty(Navigator.prototype, 'onLine', {
								configurable: true,
								get: () => online
							});
							window.__boxsetSetOnline = (next) => {
								online = next;
								window.dispatchEvent(new Event(next ? 'online' : 'offline'));
							};
						}, attribution);

						// The first dev request can trigger one dependency-optimizer reload; settle it.
						const response = await page.goto(attribution ? documentURL : origin!);
						try {
							await expect(page.getByRole('status')).toHaveText('Loading Boxset…');
							expect(await page.locator('form, input, a').count()).toBe(0);
							expect(requests.filter((request) => request.path.includes('/api/'))).toEqual([]);
						} finally {
							clearTimeout(timer);
							release();
						}
						const policy = response?.headers()['content-security-policy'];
						if (!attribution && policy) {
							expect(policy).toMatch(
								/^default-src 'self'; script-src 'self'(?: 'sha256-[A-Za-z0-9+/]{43}=')*; style-src 'self'; style-src-attr 'unsafe-hashes' 'sha256-S8qMpvofolR8Mpjy4kQvEm7m1q8clzU4dfDH0AmvZjo='; img-src 'self' data: blob:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'$/
							);
							expect(response?.headers()['content-security-policy-report-only']).toBeUndefined();
						}
						await page.waitForLoadState('networkidle');
						await diagnostic?.settle();
						if (!attribution) expect(productViolations).toEqual([]);
						if (!attribution) {
							await page.reload();
							await page.waitForLoadState('networkidle');
							expect(productViolations).toEqual([]);
						}

						const heading = page.getByRole('heading', { level: 1, name: 'Boxset' });
						await expect(heading).toBeVisible();
						if (!attribution) {
							const loaded = await page.evaluate(async () => {
								const faces = await document.fonts.load('800 24px Archivo', 'BOXSET');
								return faces.map((face) => ({ family: face.family, status: face.status }));
							});
							expect(loaded).toEqual([{ family: 'Archivo', status: 'loaded' }]);
							expect(
								await heading.evaluate((element) => getComputedStyle(element).fontVariationSettings)
							).toBe('"wdth" 70');
							const fonts = await page.evaluate(() =>
								performance
									.getEntriesByType('resource')
									.map((entry) => entry.name)
									.filter((name) => /\.woff2(?:\?|$)/.test(name))
							);
							expect(fonts).toHaveLength(1);
							expect(new URL(fonts[0]).origin).toBe(new URL(origin!).origin);
							expect(fonts[0]).toContain('archivo-latin-standard-normal');
							const css = await page.context().newCDPSession(page);
							try {
								await css.send('DOM.enable');
								await css.send('CSS.enable');
								const { root } = await css.send('DOM.getDocument');
								const { nodeId } = await css.send('DOM.querySelector', {
									nodeId: root.nodeId,
									selector: 'h1'
								});
								const rendered = await css.send('CSS.getPlatformFontsForNode', { nodeId });
								// The platform reports the file's own name table (the variable font's default
								// instance), not the CSS family: every glyph of the heading comes from it.
								expect(rendered.fonts).toEqual([
									{
										familyName: 'Archivo SemiBold',
										postScriptName: 'Archivo-SemiBold',
										isCustomFont: true,
										glyphCount: 'Boxset'.length
									}
								]);
							} finally {
								await css.detach();
							}
						}
						await settled();

						await diagnostic?.settle();
						// A deep link (query and fragment on the shell route) boots the same shell.
						if (!attribution) {
							await page.goto(new URL('?deep=link#section', origin).href);
							await page.waitForLoadState('networkidle');
						}
						await expect(heading).toBeVisible();
						await settled();
						const announcer = page.locator('#svelte-announcer');
						await expect(announcer).toHaveAttribute('aria-live', 'assertive');
						await expect(announcer).toHaveAttribute('aria-atomic', 'true');
						if (!attribution) {
							await page.evaluate(() => {
								document.documentElement.dataset.boxsetClientDocument = 'same';
								const link = document.createElement('a');
								link.href = new URL('?announcer=1', location.href).href;
								link.textContent = 'Announce route';
								document.body.append(link);
							});
							const link = page.getByRole('link', { name: 'Announce route' });
							await link.click();
							await expect(page).toHaveURL(/\?announcer=1$/);
							expect(
								await page.evaluate(() => document.documentElement.dataset.boxsetClientDocument)
							).toBe('same');
							await link.evaluate((element) => element.remove());
							await page.evaluate(() =>
								Reflect.deleteProperty(document.documentElement.dataset, 'boxsetClientDocument')
							);
							await expect(announcer).toHaveText('Boxset');
						}
						if (!attribution && policy) {
							expect(process.env.BOXSET_CSP_SOURCE_STYLE).toBeTruthy();
							await expect(announcer).toHaveAttribute(
								'style',
								process.env.BOXSET_CSP_SOURCE_STYLE!
							);
							expect(
								await announcer.evaluate((element) => getComputedStyle(element).position)
							).toBe('absolute');
							expect(await announcer.evaluate((element) => getComputedStyle(element).width)).toBe(
								'1px'
							);
						}

						// Offline: the public reconnect message and a keyboard-reachable Retry.
						await page.evaluate(() => window.__boxsetSetOnline(false));
						const retry = page.getByRole('button', { name: 'Retry' });
						await expect(retry).toBeVisible();
						await page.keyboard.press('Tab');
						await expect(retry).toBeFocused();
						expect(await retry.evaluate((element) => element.matches(':focus-visible'))).toBe(true);
						expect(await retry.evaluate((element) => element.tagName)).toBe('BUTTON');
						await retry.click();
						await expect(retry).toBeVisible();
						await page.keyboard.press('Enter');
						await expect(retry).toBeFocused();
						expect(await retry.evaluate((element) => element.matches(':focus-visible'))).toBe(true);
						const ring = await retry.evaluate((element) => getComputedStyle(element).outlineStyle);
						expect(ring).not.toBe('none');
						if (!attribution) expect(productViolations).toEqual([]);
						await page.keyboard.press('Enter');
						await expect(retry).toBeVisible();
						await page.evaluate(() => window.__boxsetSetOnline(true));
						await settled();

						await page.evaluate(async () => {
							const { promise, resolve } = Promise.withResolvers<void>();
							setTimeout(resolve, 0);
							await promise;
						});
						await diagnostic?.finish(true);
						if (!attribution) expect(productViolations).toEqual([]);
						if (attribution && index === 0) continue;

						if (!attribution && view === 'desktop') {
							// Reflow/expansion checks are not genuine browser zoom or a manual WCAG verdict.
							const widths = [
								...new Set([
									320,
									768,
									1024,
									1440,
									...Array.from({ length: 41 }, (_, i) => 320 + i * 40)
								])
							];
							for (const online of [true, false]) {
								await page.evaluate((value) => window.__boxsetSetOnline(value), online);
								await expect(page.getByRole('status')).toBeVisible();
								if (online) await settled();
								for (const width of widths) {
									await page.setViewportSize({ width, height: 900 });
									expect(
										await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth),
										`page overflow at ${width}, online=${online}`
									).toBe(true);
								}
								const original = await page
									.locator('main h1, main h2, main [role="status"] span, main button, main a')
									.allTextContents();
								for (const theme of ['light', 'dark']) {
									for (const reducedMotion of ['reduce', 'no-preference'] as const) {
										await page.emulateMedia({ reducedMotion });
										await page.evaluate(
											({ theme, original }) => {
												document.documentElement.dataset.theme = theme;
												const elements = document.querySelectorAll<HTMLElement>(
													'main h1, main h2, main [role="status"] span, main button, main a'
												);
												elements.forEach((element, index) => {
													const pseudo = original[index].replace(
														/[aeiou]/gi,
														(letter) =>
															({ a: 'á', e: 'ë', i: 'ï', o: 'ö', u: 'ü' })[letter.toLowerCase()]!
													);
													element.textContent = `[${pseudo} ${pseudo}]`;
													element.style.lineHeight = '1.5';
													element.style.letterSpacing = '0.12em';
													element.style.wordSpacing = '0.16em';
													if (element.tagName === 'SPAN')
														element.parentElement!.style.marginBottom = '2em';
												});
											},
											{ theme, original }
										);
										for (const width of [320, 768, 1024, 1440]) {
											await page.setViewportSize({ width, height: 900 });
											const fits = await page.locator('main').evaluate((main) => {
												const bounds = main.getBoundingClientRect();
												return (
													document.documentElement.scrollWidth <= innerWidth &&
													bounds.left >= 0 &&
													bounds.right <= innerWidth &&
													[...main.querySelectorAll<HTMLElement>('h1,h2,p,button,a')].every(
														(element) =>
															element.scrollWidth <= element.clientWidth &&
															element.scrollHeight <= element.clientHeight
													)
												);
											});
											expect(
												fits,
												`expanded ${theme}/${reducedMotion} at ${width}, online=${online}`
											).toBe(true);
										}
									}
								}
								await page.evaluate((original) => {
									document
										.querySelectorAll<HTMLElement>(
											'main h1, main h2, main [role="status"] span, main button, main a'
										)
										.forEach((element, index) => {
											element.textContent = original[index];
											element.style.removeProperty('line-height');
											element.style.removeProperty('letter-spacing');
											element.style.removeProperty('word-spacing');
											if (element.tagName === 'SPAN')
												element.parentElement!.style.removeProperty('margin-bottom');
										});
									Reflect.deleteProperty(document.documentElement.dataset, 'theme');
								}, original);
							}
							await page.emulateMedia({ reducedMotion: 'no-preference' });
							await page.setViewportSize(size);
							await page.evaluate(() => window.__boxsetSetOnline(true));
							await settled();
							expect(productViolations).toEqual([]);
						}

						// All product navigations and Loading/Offline/Retry measurement precede axe and labels.
						const loading = await new AxeBuilder({ page }).analyze();
						expect(loading.violations).toEqual([]);
						await page.evaluate(() => window.__boxsetSetOnline(false));
						await expect(retry).toBeVisible();
						const offline = await new AxeBuilder({ page }).analyze();
						expect(offline.violations).toEqual([]);
						await page.evaluate(() => window.__boxsetSetOnline(true));

						await settled();

						// The actual browser values, read before anything is injected.
						const actual = await page.evaluate(() => ({
							dpr: window.devicePixelRatio,
							width: window.innerWidth,
							height: window.innerHeight
						}));
						if (actual.dpr !== 1 || actual.width !== size.width || actual.height !== size.height) {
							const line = `S5-CAPTURE-VIEWPORT-MISMATCH name=${name} dpr=${actual.dpr} inner=${actual.width}x${actual.height}`;
							console.log(line);
							throw new Error(line);
						}

						// Challenge strip (66 bits: challenge MSB first, mode, viewport) and label.
						const bits = [
							...[...challenge].flatMap((digit) => {
								const value = parseInt(digit, 16);
								return [3, 2, 1, 0].map((shift) => (value >> shift) & 1);
							}),
							mode === 'preview' ? 1 : 0,
							view === 'mobile' ? 1 : 0
						];
						const label = `boxset-capture ${challenge} ${name} ${actual.width}x${actual.height}@${actual.dpr}`;
						await page.evaluate(() => window.__boxsetSetOnline(false));
						await expect(retry).toBeVisible();
						await page.keyboard.press('Tab');
						await expect(retry).toBeFocused();
						await page.keyboard.press('Enter');
						const focusedBeforeLabels = await page.evaluate(
							() => document.activeElement?.outerHTML
						);
						await page.evaluate(
							({ bits, label }) => {
								const strip = document.createElement('div');
								strip.setAttribute('aria-hidden', 'true');
								strip.dataset.boxsetCapture = 'strip';
								strip.style.cssText =
									'position:fixed;left:0;bottom:0;width:264px;height:16px;margin:0;padding:0;display:grid;grid-template-columns:repeat(33,8px);grid-template-rows:repeat(2,8px);pointer-events:none;z-index:2147483647';
								for (const bit of bits) {
									const cell = document.createElement('div');
									cell.style.cssText = `background:${bit ? 'rgb(0,0,0)' : 'rgb(255,255,255)'}`;
									strip.append(cell);
								}
								const text = document.createElement('div');
								text.setAttribute('aria-hidden', 'true');
								text.dataset.boxsetCapture = 'label';
								text.style.cssText =
									'position:fixed;left:0;bottom:16px;margin:0;padding:0;white-space:nowrap;font:11px/16px monospace;color:rgb(0,0,0);background:rgb(255,255,255);pointer-events:none;z-index:2147483647';
								text.textContent = label;
								document.body.append(strip, text);
							},
							{ bits, label }
						);

						const box = await retry.boundingBox();
						expect(await page.evaluate(() => document.activeElement?.outerHTML)).toBe(
							focusedBeforeLabels
						);
						await expect(retry).toBeFocused();
						expect(await retry.evaluate((element) => element.matches(':focus-visible'))).toBe(true);
						expect(box).not.toBeNull();
						for (const part of ['strip', 'label']) {
							const other = await page.locator(`[data-boxset-capture="${part}"]`).boundingBox();
							expect(other).not.toBeNull();
							const apart =
								box!.x + box!.width <= other!.x ||
								other!.x + other!.width <= box!.x ||
								box!.y + box!.height <= other!.y ||
								other!.y + other!.height <= box!.y;
							expect(apart, `${part} overlaps Retry`).toBe(true);
						}
						expect(
							attribution
								? problems.filter(
										(problem) =>
											!/^Applying inline style violates the following Content Security Policy directive ["']style-src 'self'["']/.test(
												problem
											)
									)
								: problems
						).toEqual([]);

						await page.screenshot({
							path: `${captureDir}/${name}.png`,
							fullPage: false,
							animations: 'disabled',
							scale: 'css'
						});
					} finally {
						try {
							await diagnostic?.finish();
						} finally {
							if (attribution) await context.close();
						}
					}
				}
			});
		});
	}
}
