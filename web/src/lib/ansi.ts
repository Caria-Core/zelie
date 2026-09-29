// Turns a console line with ANSI colour codes into pieces of text with an
// inline style. Only colours and text weight are kept; every other escape
// sequence is dropped. The text is never treated as HTML.
export type Span = { text: string; style: string };

const basic = ['#1e1e1e', '#e5544b', '#3fb56b', '#d9a441', '#5b8def', '#b67ad8', '#3bb8c4', '#cfcfcf'];
const bright = ['#6b6b6b', '#ff7a70', '#63d98d', '#f0c265', '#82a9ff', '#d39cf0', '#63d8e3', '#ffffff'];

function palette(n: number): string | null {
	if (n < 0 || n > 255) return null;
	if (n < 8) return basic[n];
	if (n < 16) return bright[n - 8];
	if (n >= 232) {
		const v = 8 + (n - 232) * 10;
		return `rgb(${v},${v},${v})`;
	}
	const c = n - 16;
	const level = (i: number) => (i === 0 ? 0 : 55 + i * 40);
	return `rgb(${level(Math.floor(c / 36))},${level(Math.floor(c / 6) % 6)},${level(c % 6)})`;
}

type Look = { fg: string | null; bg: string | null; bold: boolean; italic: boolean; underline: boolean };

const plain = (): Look => ({ fg: null, bg: null, bold: false, italic: false, underline: false });

function apply(look: Look, codes: number[]) {
	for (let i = 0; i < codes.length; i++) {
		const c = codes[i];
		if (c === 0) Object.assign(look, plain());
		else if (c === 1) look.bold = true;
		else if (c === 3) look.italic = true;
		else if (c === 4) look.underline = true;
		else if (c === 22) look.bold = false;
		else if (c === 23) look.italic = false;
		else if (c === 24) look.underline = false;
		else if (c >= 30 && c <= 37) look.fg = basic[c - 30];
		else if (c >= 90 && c <= 97) look.fg = bright[c - 90];
		else if (c >= 40 && c <= 47) look.bg = basic[c - 40];
		else if (c >= 100 && c <= 107) look.bg = bright[c - 100];
		else if (c === 39) look.fg = null;
		else if (c === 49) look.bg = null;
		else if (c === 38 || c === 48) {
			let colour: string | null = null;
			if (codes[i + 1] === 5) {
				colour = palette(codes[i + 2]);
				i += 2;
			} else if (codes[i + 1] === 2) {
				const [r, g, b] = codes.slice(i + 2, i + 5);
				if ([r, g, b].every((x) => x >= 0 && x <= 255)) colour = `rgb(${r},${g},${b})`;
				i += 4;
			} else break;
			if (c === 38) look.fg = colour;
			else look.bg = colour;
		}
	}
}

function styleOf(look: Look): string {
	let s = '';
	if (look.fg) s += `color:${look.fg};`;
	if (look.bg) s += `background:${look.bg};`;
	if (look.bold) s += 'font-weight:600;';
	if (look.italic) s += 'font-style:italic;';
	if (look.underline) s += 'text-decoration:underline;';
	return s;
}

// CSI sequences (the last character says what it does), operating system
// commands, and two-character escapes.
const escapes = /\x1b\[([0-9;:]*)([\x20-\x2f]*[\x40-\x7e])|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)?|\x1b[\x40-\x5f]/g;

export function parseAnsi(line: string): Span[] {
	const spans: Span[] = [];
	const look = plain();
	let last = 0;
	const push = (text: string) => {
		text = text.replace(/[\r\x00-\x08\x0b-\x1a\x1c-\x1f]/g, '');
		if (text) spans.push({ text, style: styleOf(look) });
	};
	for (const m of line.matchAll(escapes)) {
		push(line.slice(last, m.index));
		last = m.index + m[0].length;
		if (m[2] === 'm') apply(look, m[1] === '' ? [0] : m[1].split(/[;:]/).map((x) => (x === '' ? 0 : Number(x))));
	}
	push(line.slice(last));
	return spans;
}
