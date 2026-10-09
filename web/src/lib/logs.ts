// The core starts a container log it has cleared with a marker line (see
// IsLogCut in the Go code). The interface shows its own words in its place.
const cutLine = /^\[zelie:log-cut \d+\]$/;
const cutLines = /^\[zelie:log-cut \d+\]$/gm;

export function isLogCut(line: string): boolean {
	return cutLine.test(line);
}

// withCutNotes replaces the marker lines in a block of log output with note.
export function withCutNotes(text: string, note: string): string {
	return text.replace(cutLines, () => note);
}
