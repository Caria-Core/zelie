import en from './locales/en';

export type Key = keyof typeof en;

const messages: Record<Key, string> = en;

// t returns the text for key, filling in {name} placeholders from vars.
export function t(key: Key, vars?: Record<string, string | number>): string {
	let s = messages[key] ?? key;
	if (vars) for (const [k, v] of Object.entries(vars)) s = s.replaceAll(`{${k}}`, String(v));
	return s;
}
