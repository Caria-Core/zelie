import { untrack } from 'svelte';

// Undo for a form. It watches a snapshot of the form's values and keeps the
// earlier ones, so Undo steps back one edit at a time. Keystrokes typed in
// one go count as a single edit.
const limit = 50;
const burst = 700;

export function editHistory<T>(read: () => T, write: (value: T) => void) {
	let base = $state('');
	let now = '';
	let last = 0;
	const past = $state<string[]>([]);

	function remember() {
		past.push(now);
		if (past.length > limit) past.shift();
	}

	$effect(() => {
		const next = JSON.stringify(read());
		untrack(() => {
			if (now !== '' && next !== now) {
				const at = Date.now();
				if (at - last > burst) remember();
				last = at;
			}
			now = next;
		});
	});

	return {
		get dirty() {
			return JSON.stringify(read()) !== base;
		},
		get canUndo() {
			return past.length > 0;
		},
		// Call after the form is filled from the server or saved: what is in it
		// now is the last save.
		rebase() {
			base = now = JSON.stringify(read());
			past.length = 0;
		},
		undo() {
			const before = past.pop();
			if (before === undefined) return;
			now = before;
			write(JSON.parse(before));
		},
		// Back to the last save. Undo brings the edits back.
		revert() {
			if (now === base) return;
			remember();
			last = 0;
			now = base;
			write(JSON.parse(base));
		}
	};
}
