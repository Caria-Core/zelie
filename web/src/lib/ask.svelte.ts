// A question the user answers before something that is hard to take back.
// The dialog in the app layout shows it.
export type Question = { title: string; text?: string; link?: { href: string; label: string }; action: string; danger?: boolean };

export const asking = $state<{ question: Question | null; answer: ((ok: boolean) => void) | null }>({
	question: null,
	answer: null
});

// ask resolves with true when the user takes the action, and false when they
// cancel or close the dialog.
export function ask(question: Question): Promise<boolean> {
	asking.answer?.(false);
	return new Promise((resolve) => {
		asking.question = question;
		asking.answer = (ok) => {
			asking.question = null;
			asking.answer = null;
			resolve(ok);
		};
	});
}
