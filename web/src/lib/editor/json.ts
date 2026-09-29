// What is wrong with a JSON text, or null when it parses. Kept apart from
// the editor so that saving can check without loading it.
export type Problem = { line: number; column: number; message: string };
export type JsonProblem = Problem & { pos: number };

export function jsonProblem(text: string): JsonProblem | null {
	if (!text.trim()) return null;
	try {
		JSON.parse(text);
		return null;
	} catch (err) {
		const raw = err instanceof Error ? err.message : String(err);
		let pos = -1;
		let line = 0;
		let column = 0;
		// Browsers word this differently: some give an offset, some a line
		// and a column, some both.
		const at = /position (\d+)/.exec(raw);
		const lc = /line (\d+) column (\d+)/.exec(raw);
		if (at) pos = Number(at[1]);
		if (lc) {
			line = Number(lc[1]);
			column = Number(lc[2]);
		}
		if (pos < 0 && line > 0) pos = offsetOf(text, line, column);
		if (pos < 0) pos = /unexpected end/i.test(raw) ? text.length : 0;
		pos = Math.min(pos, text.length);
		if (!line) ({ line, column } = lineColumn(text, pos));
		const message = raw
			.replace(/^JSON\.parse: /, '')
			.replace(/\s*\(line \d+ column \d+\)/, '')
			.replace(/\s+(in JSON )?at (position \d+|line \d+ column \d+)( of the JSON data)?/, '')
			.replace(/ in JSON$/, '');
		return { pos, line, column, message };
	}
}

function lineColumn(text: string, pos: number): { line: number; column: number } {
	let line = 1;
	let start = 0;
	for (let i = text.indexOf('\n'); i >= 0 && i < pos; i = text.indexOf('\n', i + 1)) {
		line++;
		start = i + 1;
	}
	return { line, column: pos - start + 1 };
}

function offsetOf(text: string, line: number, column: number): number {
	let start = 0;
	for (let n = 1; n < line; n++) {
		const i = text.indexOf('\n', start);
		if (i < 0) return text.length;
		start = i + 1;
	}
	return start + Math.max(column - 1, 0);
}
