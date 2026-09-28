import en from './locales/en';
import enMessages from './locales/messages.en';

// The interface's own text, and the messages the server sends (generated
// from the Go code). A new language is a file of each, typed
// Record<Key, string>, so a missing key fails the build.
const english = { ...en, ...enMessages };

export type Key = keyof typeof english;

const messages: Record<Key, string> = english;
const locale = 'en';

type Vars = Record<string, string | number>;

// A message the server sends: its code, the values for its placeholders,
// and its English text for when this language has no such code.
export type Msg = { code: string; params?: Record<string, unknown>; text: string };

// t returns the text for key. {name} is replaced by vars.name, and
// {n, plural, one {…} other {…}} picks the form for the number n in this
// language (zero, one, two, few, many, other, or =0 for an exact value),
// with # standing for the number.
export function t(key: Key, vars?: Vars): string {
	return format(messages[key] ?? key, vars);
}

// say shows a message from the server in the user's language.
export function say(m: Msg): string {
	const key = ('msg.' + m.code) as Key;
	if (!(key in messages)) return m.text;
	const vars: Vars = {};
	for (const [k, v] of Object.entries(m.params ?? {})) vars[k] = typeof v === 'number' ? v : String(v);
	return format(messages[key], vars);
}

const plurals = new Intl.PluralRules(locale);
const lists = new Intl.ListFormat(locale, { type: 'conjunction' });

// list joins names the way this language does: a, b and c.
export function list(items: string[]): string {
	return lists.format(items);
}

function format(s: string, vars: Vars = {}): string {
	let out = '';
	let i = 0;
	while (i < s.length) {
		const open = s.indexOf('{', i);
		if (open < 0) break;
		const close = matching(s, open);
		if (close < 0) break;
		out += s.slice(i, open) + part(s.slice(open + 1, close), vars);
		i = close + 1;
	}
	return out + s.slice(i);
}

function part(inner: string, vars: Vars): string {
	const m = /^\s*(\w+)\s*,\s*plural\s*,(.*)$/s.exec(inner);
	if (!m) return inner in vars ? String(vars[inner]) : '{' + inner + '}';
	const n = Number(vars[m[1]]);
	const forms = new Map<string, string>();
	const re = /\s*(=\d+|zero|one|two|few|many|other)\s*\{/g;
	let f: RegExpExecArray | null;
	while ((f = re.exec(m[2]))) {
		const start = f.index + f[0].length - 1;
		const end = matching(m[2], start);
		if (end < 0) break;
		forms.set(f[1], m[2].slice(start + 1, end));
		re.lastIndex = end + 1;
	}
	const form = forms.get('=' + n) ?? forms.get(plurals.select(n)) ?? forms.get('other') ?? '';
	return format(form.replaceAll('#', String(n)), vars);
}

// matching finds the brace that closes the one at open.
function matching(s: string, open: number): number {
	let depth = 0;
	for (let i = open; i < s.length; i++) {
		if (s[i] === '{') depth++;
		else if (s[i] === '}' && --depth === 0) return i;
	}
	return -1;
}
