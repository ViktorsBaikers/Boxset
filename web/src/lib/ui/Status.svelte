<script lang="ts">
	import type { Snippet } from 'svelte';

	type Props = { tone?: 'neutral' | 'fault' | 'done'; children: Snippet };

	let { tone = 'neutral', children }: Props = $props();
</script>

<!-- A live region that carries its state in text and in a shape (a dashed ring for
     information, a triangle for a fault, a tick when done). -->
<p
	role="status"
	class={[
		'flex items-start gap-3',
		tone === 'fault' ? 'text-fault' : tone === 'done' ? 'text-ink' : 'text-ink-muted'
	]}
>
	<svg
		aria-hidden="true"
		focusable="false"
		viewBox="0 0 12 12"
		class="mt-1 size-3 shrink-0"
		fill="none"
		stroke="currentColor"
		stroke-width="1.5"
	>
		{#if tone === 'fault'}
			<path d="M6 1.5 11 10.5H1Z" stroke-linejoin="round" />
		{:else if tone === 'done'}
			<path d="M2 6.5 4.75 9.25 10 3" stroke-linecap="round" stroke-linejoin="round" />
		{:else}
			<circle cx="6" cy="6" r="4.25" stroke-dasharray="2 2" />
		{/if}
	</svg>
	<span>{@render children()}</span>
</p>
