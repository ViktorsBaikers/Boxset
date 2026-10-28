<script lang="ts">
	import type { Snippet } from 'svelte';
	import type { HTMLButtonAttributes } from 'svelte/elements';

	type Props = Omit<HTMLButtonAttributes, 'class' | 'children'> & {
		variant?: 'default' | 'primary' | 'ghost';
		children: Snippet;
	};

	let { variant = 'default', type = 'button', children, ...rest }: Props = $props();

	const variants = {
		default: 'border-rule bg-board text-ink hover:border-rule-strong',
		primary: 'border-ink bg-ink text-ink-inverse hover:bg-ink/88',
		ghost: 'border-transparent bg-transparent text-ink-muted hover:bg-board hover:text-ink'
	};
</script>

<button
	{type}
	class={[
		'inline-flex items-center justify-center rounded-control border px-3 py-2 text-control',
		'transition-colors motion-reduce:transition-none',
		'disabled:cursor-not-allowed disabled:opacity-60',
		variants[variant]
	]}
	{...rest}
>
	{@render children()}
</button>
