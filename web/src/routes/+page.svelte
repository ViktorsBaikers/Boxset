<script lang="ts">
	import Probe from '../lib/toolchain-probe/Probe.svelte';
	import { m } from '../../.svelte-kit/paraglide/messages.js';
</script>

<svelte:head>
	<title>{m.probe_title()}</title>
</svelte:head>

<main>
	<Probe />
</main>
