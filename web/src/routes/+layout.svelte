<script lang="ts">
	import { QueryClient, QueryClientProvider } from '@tanstack/svelte-query';
	import { onMount, type Snippet } from 'svelte';
	import '../app.css';
	import { ApiError, type EventsIdentity } from '../lib/api';
	import { leaveEvents, startEvents, stopEvents } from '../lib/events/client';
	import type { Access } from '../lib/session';

	let { children }: { children: Snippet } = $props();

	// Server state lives only here. Mutations are never retried; a read retries once on a
	// network error.
	const queryClient = new QueryClient({
		defaultOptions: { queries: { retry: 1, gcTime: 0 }, mutations: { retry: false, gcTime: 0 } }
	});

	// The stream belongs to the app, not to a route. This tab confirms only with the
	// values its own session response carries, and the client invalidates through this
	// one QueryClient, never a second instance.
	onMount(() => {
		const projection = () => {
			const queries = queryClient.getQueryCache().findAll({ queryKey: ['access'] });
			for (const query of queries) {
				const data = query.state.data as Access | undefined;
				const session = data?.state === 'ready' ? data.session : undefined;
				if (!session?.installId) continue;
				const identity: EventsIdentity = {
					installId: session.installId,
					userId: session.user?.id ?? 0,
					restoreEpoch: session.restoreEpoch
				};
				startEvents(identity, queryClient);
				return;
			}
			// A confirmation in flight is not a lost identity: the stream waits for the
			// authoritative answer instead of being ended by every refetch it starts.
			if (queries.some((query) => query.state.fetchStatus === 'fetching')) return;
			// A settled authorization refusal is the only loss that ends the shared
			// stream for every tab. No access query left means this tab only left its
			// route, and a public lifecycle state or a network error is not a loss.
			if (
				queries.some((query) => {
					const error = query.state.error;
					return error instanceof ApiError && (error.status === 401 || error.status === 403);
				})
			)
				stopEvents();
			else leaveEvents();
		};
		const unsubscribe = queryClient.getQueryCache().subscribe(projection);
		// A closing document is this tab leaving: without the signal its port would stay
		// confirmed in the worker, so the installation's stream could outlive every tab.
		const unload = () => leaveEvents();
		window.addEventListener('pagehide', unload);
		return () => {
			window.removeEventListener('pagehide', unload);
			unsubscribe();
		};
	});
</script>

<QueryClientProvider client={queryClient}>
	{@render children()}
</QueryClientProvider>
