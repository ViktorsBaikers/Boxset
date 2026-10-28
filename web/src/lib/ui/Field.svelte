<script lang="ts">
	import type { HTMLInputAttributes } from 'svelte/elements';

	type Props = Omit<HTMLInputAttributes, 'class' | 'id' | 'aria-describedby' | 'aria-invalid'> & {
		id: string;
		label: string;
		description?: string;
		error?: string;
	};

	let { id, label, description, error, ...rest }: Props = $props();

	const describedBy = $derived(
		[description && `${id}-description`, error && `${id}-error`].filter(Boolean).join(' ') ||
			undefined
	);
</script>

<!-- A labelled native input. Its help and its error are linked to it; the error carries a
     shape as well as colour. -->
<div class="flex flex-col gap-1">
	<label for={id} class="text-control text-ink">{label}</label>
	{#if description}
		<p id="{id}-description" class="text-data text-ink-muted">{description}</p>
	{/if}
	<input
		{id}
		aria-invalid={error ? 'true' : undefined}
		aria-describedby={describedBy}
		class={[
			'w-full rounded-control border bg-board px-3 py-2 text-body text-ink',
			'placeholder:text-ink-dim',
			error ? 'border-fault' : 'border-rule hover:border-rule-strong'
		]}
		{...rest}
	/>
	{#if error}
		<p id="{id}-error" class="flex items-start gap-2 text-data text-fault">
			<svg
				aria-hidden="true"
				focusable="false"
				viewBox="0 0 12 12"
				class="mt-1 size-3 shrink-0"
				fill="none"
				stroke="currentColor"
				stroke-width="1.5"
			>
				<path d="M6 1.5 11 10.5H1Z" stroke-linejoin="round" />
			</svg>
			<span>{error}</span>
		</p>
	{/if}
</div>
