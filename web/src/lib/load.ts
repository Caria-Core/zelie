// How full a limit is, as a text colour class: green below 70 %, amber
// from 70 %, red from 90 %.
export function loadClass(used: number | undefined, limit: number | undefined): string {
	if (used === undefined || !limit) return '';
	const share = used / limit;
	if (share >= 0.9) return 'text-danger';
	if (share >= 0.7) return 'text-warn';
	return 'text-ok';
}
