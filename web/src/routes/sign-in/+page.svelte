<script lang="ts">
	import {
		createForm,
		Field as FormField,
		Form,
		setInput,
		setErrors,
		focus
	} from '@formisch/svelte';
	import { createMutation, useQueryClient } from '@tanstack/svelte-query';
	import * as v from 'valibot';
	import { goto } from '$app/navigation';
	import { resolve } from '$app/paths';
	import { ApiError, login, type Credentials } from '../../lib/api';
	import Button from '../../lib/ui/Button.svelte';
	import Field from '../../lib/ui/Field.svelte';
	import Status from '../../lib/ui/Status.svelte';
	import { m } from '../../../.svelte-kit/paraglide/messages.js';

	const queryClient = useQueryClient();
	const form = createForm({
		schema: v.object({
			username: v.pipe(
				v.string(),
				v.nonEmpty(m.setup_username_required()),
				v.check((s) => [...s].length <= 64, m.setup_username_too_long())
			),
			password: v.pipe(
				v.string(),
				v.minBytes(12, m.setup_password_length()),
				v.maxBytes(1024, m.setup_password_length())
			)
		})
	});
	const signIn = createMutation<void, unknown, Credentials>(() => ({
		mutationFn: login,
		networkMode: 'always',
		retry: false,
		onSuccess: async () => {
			queryClient.clear();
			await goto(resolve('/'));
		},
		onError: (error) => {
			if (error instanceof ApiError && error.code === 'validation_failed') {
				for (const field of error.fields)
					setErrors(form, { path: [field], errors: [m.setup_check_fields()] });
				if (error.fields[0]) focus(form, { path: [error.fields[0]] });
			}
		},
		onSettled: (_data, _error, input) => {
			input.password = '';
			setInput(form, { path: ['password'], input: '' });
		}
	}));
	function submit(input: Credentials) {
		if (!signIn.isPending) signIn.mutate(input);
	}
	const failure = $derived.by(() => {
		const code = signIn.error instanceof ApiError ? signIn.error.code : '';
		switch (code) {
			case 'credentials_invalid':
				return m.session_wrong_credentials();
			case 'offline':
				return m.setup_offline();
			case 'login_throttled':
			case 'hash_busy':
				return m.setup_busy();
			case 'host_not_allowed':
				return m.session_host_denied();
			case 'validation_failed':
				return m.setup_check_fields();
			case 'cross_origin_denied':
				return m.session_origin_denied();
			case 'starting':
			case 'recovering':
			case 'draining':
				return m.setup_unavailable();
			default:
				return m.session_failed();
		}
	});
</script>

<svelte:head><title>{m.session_sign_in()} · {m.app_name()}</title></svelte:head>

<main class="mx-auto flex w-full max-w-prose flex-col gap-6 px-4 py-8">
	<header class="flex items-center gap-3 text-ink">
		<svg aria-hidden="true" focusable="false" viewBox="0 0 64 64" class="size-8 fill-current">
			<rect x="4" y="55" width="56" height="4" rx="2" />
			<rect x="7" y="17" width="9" height="38" rx="1.5" transform="rotate(17 7 55)" />
			<path fill-rule="evenodd" d="M27.5 14h9v41h-9zM27.5 20h9v1.8h-9z" />
			<path fill-rule="evenodd" d="M38.5 9h9v46h-9zM38.5 15h9v1.8h-9z" />
			<path fill-rule="evenodd" d="M49.5 18h9v37h-9zM49.5 24h9v1.8h-9z" />
		</svg>
		<p class="wordmark text-wordmark uppercase">{m.app_name()}</p>
	</header>
	<section class="flex flex-col gap-6" aria-labelledby="sign-in-title">
		<h1 id="sign-in-title" class="text-headline">{m.session_sign_in()}</h1>
		<Form of={form} onsubmit={submit} class="flex flex-col gap-4">
			<FormField of={form} path={['username']}>
				{#snippet children(field)}
					<Field
						{...field.props}
						id="sign-in-username"
						label={m.setup_username_label()}
						type="text"
						autocomplete="username"
						autocapitalize="none"
						spellcheck="false"
						value={field.input}
						error={field.errors?.[0]}
					/>
				{/snippet}
			</FormField>
			<FormField of={form} path={['password']}>
				{#snippet children(field)}
					<Field
						{...field.props}
						id="sign-in-password"
						label={m.setup_password_label()}
						type="password"
						autocomplete="current-password"
						value={field.input}
						error={field.errors?.[0]}
					/>
				{/snippet}
			</FormField>
			{#if signIn.isPending}<Status>{m.session_pending()}</Status>
			{:else if signIn.isError}<Status tone="fault">{failure}</Status>{/if}
			<div>
				<Button type="submit" variant="primary" disabled={signIn.isPending}
					>{m.session_sign_in()}</Button
				>
			</div>
		</Form>
		<a class="text-control text-ink underline" href={resolve('/setup')}
			>{m.session_first_access()}</a
		>
	</section>
</main>
