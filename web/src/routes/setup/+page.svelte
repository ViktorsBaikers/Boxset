<script lang="ts">
	import {
		createForm,
		Field as FormField,
		Form,
		focus,
		setErrors,
		setInput,
		validate
	} from '@formisch/svelte';
	import { createMutation } from '@tanstack/svelte-query';
	import * as v from 'valibot';
	import { resolve } from '$app/paths';
	import { ApiError, setup, type User } from '../../lib/api';
	import Button from '../../lib/ui/Button.svelte';
	import Field from '../../lib/ui/Field.svelte';
	import Status from '../../lib/ui/Status.svelte';
	import { m } from '../../../.svelte-kit/paraglide/messages.js';

	// The server writes which command prints the token where it runs; the token itself never
	// reaches this page except as the operator types or pastes it.
	const command =
		document.documentElement.dataset.boxsetEnv === 'container'
			? 'docker exec -it <container> /boxset setup-token'
			: 'boxset setup-token';

	// Only the public username is retained for field-specific retry validation.
	let rejectedUsername: string | undefined;
	const SetupSchema = v.object({
		token: v.pipe(v.string(), v.trim(), v.nonEmpty(m.setup_token_required())),
		username: v.pipe(
			v.string(),
			v.nonEmpty(m.setup_username_required()),
			v.check((s) => [...s].length <= 64, m.setup_username_too_long()),
			v.check((s) => s !== rejectedUsername, m.setup_username_invalid())
		),
		password: v.pipe(
			v.string(),
			v.minBytes(12, m.setup_password_length()),
			v.maxBytes(1024, m.setup_password_length())
		)
	});
	type SetupInput = v.InferOutput<typeof SetupSchema>;

	const form = createForm({ schema: SetupSchema });

	const creation = createMutation<User, unknown, SetupInput>(() => ({
		mutationFn: ({ token, username, password }) => setup(token, { username, password }),
		retry: false,
		onSuccess: clearSecrets,
		onError: handleFailure,
		onSettled: (_data, _error, input) => {
			input.token = '';
			input.password = '';
		}
	}));

	function clearSecrets() {
		setInput(form, { path: ['token'], input: '' });
		setInput(form, { path: ['password'], input: '' });
	}

	function submit(input: SetupInput) {
		if (creation.isPending) return;
		creation.mutate(input);
	}

	async function handleFailure(error: unknown, input: SetupInput) {
		setInput(form, { path: ['password'], input: '' });
		if (!(error instanceof ApiError)) return;
		if (error.code === 'setup_completed') {
			clearSecrets();
			return;
		}
		if (error.code !== 'validation_failed') return;
		if (error.fields.includes('username')) rejectedUsername = input.username;
		await validate(form);
		const fields = (['username', 'password'] as const).filter((field) =>
			error.fields.includes(field)
		);
		for (const field of fields) {
			if (field === 'username') continue; // schema validation owns this retry error
			setErrors(form, {
				path: [field],
				errors: [m.setup_password_length()]
			});
		}
		if (fields[0]) focus(form, { path: [fields[0]] });
	}

	const failure = $derived.by(() => {
		const code = creation.error instanceof ApiError ? creation.error.code : 'unexpected';
		switch (code) {
			case 'setup_credential_invalid':
				return m.setup_credential_invalid();
			case 'setup_throttled':
				return m.setup_throttled();
			case 'hash_busy':
				return m.setup_busy();
			case 'host_not_allowed':
				return m.setup_host_not_allowed();
			case 'validation_failed':
				return m.setup_check_fields();
			case 'recovering':
			case 'draining':
				return m.setup_unavailable();
			case 'offline':
				return m.setup_offline();
		}
		return m.setup_failed();
	});
	const completed = $derived(
		creation.error instanceof ApiError && creation.error.code === 'setup_completed'
	);
</script>

<svelte:head>
	<title>{m.setup_page_title()}</title>
</svelte:head>

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

	{#if creation.isSuccess}
		<section class="flex flex-col items-start gap-3" aria-labelledby="setup-done">
			<h1 id="setup-done" class="text-headline">{m.setup_created_title()}</h1>
			<Status tone="done">{m.setup_created_message({ username: creation.data.username })}</Status>
			<a class="text-control text-ink underline" href={resolve('/')}>{m.setup_open_boxset()}</a>
		</section>
	{:else if completed}
		<section class="flex flex-col items-start gap-3" aria-labelledby="setup-closed">
			<h1 id="setup-closed" class="text-headline">{m.setup_completed_title()}</h1>
			<Status>{m.setup_completed_message()}</Status>
			<a class="text-control text-ink underline" href={resolve('/')}>{m.setup_open_boxset()}</a>
		</section>
	{:else}
		<section class="flex flex-col gap-6" aria-labelledby="setup-title">
			<div class="flex flex-col gap-3">
				<h1 id="setup-title" class="text-headline">{m.setup_title()}</h1>
				<p class="text-ink-muted">{m.setup_intro()}</p>
				<p class="text-ink-muted">{m.setup_token_help()}</p>
				<code
					class="rounded-control border border-rule bg-board px-3 py-2 text-data wrap-anywhere text-ink"
					>{command}</code
				>
			</div>

			<Form of={form} onsubmit={submit} class="flex flex-col gap-4">
				<FormField of={form} path={['token']}>
					{#snippet children(field)}
						<Field
							{...field.props}
							id="setup-token"
							label={m.setup_token_label()}
							type="password"
							autocomplete="off"
							spellcheck="false"
							value={field.input}
							error={field.errors?.[0]}
						/>
					{/snippet}
				</FormField>
				<FormField of={form} path={['username']}>
					{#snippet children(field)}
						<Field
							{...field.props}
							id="setup-username"
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
							id="setup-password"
							label={m.setup_password_label()}
							description={m.setup_password_help()}
							type="password"
							autocomplete="new-password"
							value={field.input}
							error={field.errors?.[0]}
						/>
					{/snippet}
				</FormField>

				{#if creation.isPending}
					<Status>{m.setup_pending()}</Status>
				{:else if creation.isError}
					<Status tone="fault">{failure}</Status>
				{/if}

				<div>
					<Button type="submit" variant="primary" disabled={creation.isPending}>
						{m.setup_submit()}
					</Button>
				</div>
			</Form>
		</section>
	{/if}
</main>
