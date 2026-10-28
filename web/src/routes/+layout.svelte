<script lang="ts">
	import { QueryClient, QueryClientProvider } from '@tanstack/svelte-query';
	import type { Snippet } from 'svelte';
	import '../app.css';

	let { children }: { children: Snippet } = $props();

	// Server state lives only here. Mutations are never retried; a read retries once on a
	// network error.
	const queryClient = new QueryClient({
		defaultOptions: { queries: { retry: 1, gcTime: 0 }, mutations: { retry: false, gcTime: 0 } }
	});
</script>

<QueryClientProvider client={queryClient}>
	{@render children()}
</QueryClientProvider>
