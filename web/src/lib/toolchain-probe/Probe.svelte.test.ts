import { expect, test } from 'vitest';
import { page, userEvent } from 'vitest/browser';
import { render } from 'vitest-browser-svelte';
import Probe from './Probe.svelte';

test('renders a labelled native button with the default count 0', async () => {
	await render(Probe);

	const button = page.getByRole('button', { name: 'Add one activation' });
	await expect.element(button).toBeVisible();
	expect(button.element().tagName).toBe('BUTTON');
	expect(navigator.userAgent).toContain('HeadlessChrome');
	await expect.element(page.getByRole('status')).toHaveTextContent('0');
	await expect.element(page.getByRole('paragraph')).toHaveTextContent('Activations: 0');
});

test('pointer then keyboard activation raise the count with visible focus', async () => {
	await render(Probe);

	const button = page.getByRole('button', { name: 'Add one activation' });
	const count = page.getByRole('status');

	await button.click();
	await expect.element(count).toHaveTextContent('1');

	await userEvent.keyboard('{Enter}');
	await expect.element(count).toHaveTextContent('2');

	await expect.element(button).toHaveFocus();
	const element = button.element();
	expect(element.matches(':focus-visible')).toBe(true);
	expect(getComputedStyle(element).outlineStyle).not.toBe('none');
});
