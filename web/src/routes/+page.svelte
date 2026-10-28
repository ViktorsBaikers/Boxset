<script lang="ts">
	import { createQuery, createMutation, useQueryClient } from '@tanstack/svelte-query';
	import { resolve } from '$app/paths';
	import { goto } from '$app/navigation';
	import { ApiError, logout } from '../lib/api';
	import { access } from '../lib/session';
	import Button from '../lib/ui/Button.svelte';
	import Status from '../lib/ui/Status.svelte';
	import { m } from '../../.svelte-kit/paraglide/messages.js';

	let online = $state(navigator.onLine);
	let visible = $state(document.visibilityState === 'visible');
	let confirmation = $state(0);
	let ending = $state(false);
	// Sticky warning fact: a fresh authoritative session decides it, so it
	// survives pending, offline, restricted and logout-error states without
	// keeping any private user data.
	let localBypass = $state(false);
	const client = useQueryClient();
	const current = createQuery(() => ({
		queryKey: ['access', confirmation],
		queryFn: ({ signal }) => access(signal),
		enabled: online && visible && !ending,
		networkMode: 'always',
		retry: false,
		gcTime: 0,
		staleTime: 0,
		refetchOnWindowFocus: 'always'
	}));
	const signOut = createMutation(() => ({
		mutationFn: logout,
		networkMode: 'always',
		onMutate: () => {
			ending = true;
			void client.cancelQueries({ queryKey: ['access'] });
			client.removeQueries();
		},
		onSuccess: () => {
			client.clear();
			return goto(resolve('/sign-in'));
		},
		retry: false
	}));
	const failure = $derived.by(() => {
		const code = current.error instanceof ApiError ? current.error.code : '';
		if (code === 'unauthenticated' || code === 'credential_invalid') return m.session_required();
		if (code === 'forbidden') return m.session_denied();
		return m.offline_message();
	});
	const lifecycleMessage = $derived.by(() => {
		switch (current.data?.state) {
			case 'starting':
				return m.session_starting();
			case 'recovering':
				return m.session_recovering();
			case 'draining':
				return m.session_draining();
			default:
				return m.loading_status();
		}
	});

	const bypassOnly = $derived(current.data?.session?.credential === 'local_bypass');
	$effect(() => {
		const confirmed = current.data?.session;
		if (confirmed) localBypass = confirmed.localBypassEnabled;
	});

	function retry() {
		online = navigator.onLine;
		void client.cancelQueries({ queryKey: ['access'] });
		confirmation++;
	}
	function visibility() {
		visible = document.visibilityState === 'visible';
		retry();
	}
</script>

<svelte:head>
	<title>{m.app_name()}</title>
</svelte:head>

<svelte:window ononline={retry} onoffline={retry} />
<svelte:document onvisibilitychange={visibility} />

<main class="mx-auto flex max-w-prose flex-col gap-6 px-4 py-8">
	<header class="flex items-center gap-3 text-ink">
		<svg aria-hidden="true" focusable="false" viewBox="0 0 64 64" class="size-8 fill-current">
			<rect x="4" y="55" width="56" height="4" rx="2" />
			<rect x="7" y="17" width="9" height="38" rx="1.5" transform="rotate(17 7 55)" />
			<path fill-rule="evenodd" d="M27.5 14h9v41h-9zM27.5 20h9v1.8h-9z" />
			<path fill-rule="evenodd" d="M38.5 9h9v46h-9zM38.5 15h9v1.8h-9z" />
			<path fill-rule="evenodd" d="M49.5 18h9v37h-9zM49.5 24h9v1.8h-9z" />
		</svg>
		<h1 class="wordmark text-wordmark uppercase">{m.app_name()}</h1>
	</header>

	{#if localBypass}
		<section class="flex flex-col items-start gap-2" aria-labelledby="local-access-title">
			<h2 id="local-access-title" class="text-title">{m.session_local_access_warning_title()}</h2>
			<p class="text-ink-muted">{m.session_local_access_warning()}</p>
		</section>
	{/if}

	{#if !online}
		<section class="flex flex-col items-start gap-3" aria-labelledby="offline-title">
			<h2 id="offline-title" class="text-title">{m.offline_title()}</h2>
			<Status tone="fault">{m.offline_message()}</Status>
			<Button variant="primary" onclick={retry}>{m.offline_retry()}</Button>
		</section>
	{:else if signOut.isError}
		<Status tone="fault">{m.session_sign_out_failed()}</Status>
		<Button onclick={() => signOut.mutate()}>{m.session_sign_out()}</Button>
		<a class="text-control text-ink underline" href={resolve('/sign-in')}>{m.session_sign_in()}</a>
	{:else if !visible || ending || current.isPending || current.isFetching}
		<Status>{m.loading_status()}</Status>
	{:else if current.isError}
		<Status tone="fault">{failure}</Status>
		<a class="text-control text-ink underline" href={resolve('/sign-in')}>{m.session_sign_in()}</a>
		<Button onclick={retry}>{m.offline_retry()}</Button>
	{:else if current.data?.state === 'ready' && current.data.session && current.data.status}
		<Status tone="done">{m.session_ready()}</Status>
		{#if current.data.session.user}
			<p class="wrap-anywhere text-ink-muted">{current.data.session.user.username}</p>
		{:else}
			<p class="text-ink-muted">{m.session_local_access()}</p>
		{/if}
		{#if current.data.session.canLogout}
			<Button onclick={() => signOut.mutate()} disabled={signOut.isPending}
				>{m.session_sign_out()}</Button
			>
		{/if}
		{#if bypassOnly}
			<a class="text-control text-ink underline" href={resolve('/sign-in')}>{m.session_sign_in()}</a
			>
		{/if}
	{:else}
		<Status>{lifecycleMessage}</Status>
		<Button onclick={retry}>{m.offline_retry()}</Button>
	{/if}
</main>
