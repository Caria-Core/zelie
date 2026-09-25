import { ApiError } from './api';
import { Cancelled } from './errors';

// The dialog in the app layout watches this. answer is set while it is open.
export const confirming = $state<{ answer: ((ok: boolean) => void) | null }>({ answer: null });

// sensitive runs fn, and if the server first wants the user to confirm it is
// them, asks and runs fn once more.
export async function sensitive<T>(fn: () => Promise<T>): Promise<T> {
	try {
		return await fn();
	} catch (e) {
		if (!(e instanceof ApiError && e.confirm)) throw e;
	}
	const ok = await new Promise<boolean>((resolve) => (confirming.answer = resolve));
	confirming.answer = null;
	if (!ok) throw new Cancelled();
	return fn();
}
