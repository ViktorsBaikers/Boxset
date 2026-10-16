<script lang="ts">
	import { m } from '../../../.svelte-kit/paraglide/messages.js';

	let count = $state(0);
</script>

<section aria-labelledby="probe-title">
	<h1 id="probe-title">{m.probe_title()}</h1>
	<button type="button" onclick={() => count++}>{m.probe_button_label()}</button>
	<p>{m.probe_count_label()}: <output>{count}</output></p>
</section>
