import { t } from './i18n';

const day = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium' });
const relative = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' });

export function date(iso: string): string {
	return day.format(new Date(iso));
}

// ago says how long since iso in the largest unit that fits.
export function ago(iso: string): string {
	const s = (Date.now() - new Date(iso).getTime()) / 1000;
	if (s < 60) return t('time.now');
	for (const [unit, size] of [
		['day', 86400],
		['hour', 3600],
		['minute', 60]
	] as const) {
		if (s >= size) return relative.format(-Math.floor(s / size), unit);
	}
	return t('time.now');
}

// device turns a user agent into something like "Firefox on macOS". It is a
// guess meant for people, and a wrong guess only makes a label less helpful.
export function device(agent: string): string {
	const browser =
		[
			['Edg/', 'Edge'],
			['OPR/', 'Opera'],
			['Firefox/', 'Firefox'],
			['Chrome/', 'Chrome'],
			['Safari/', 'Safari']
		].find(([k]) => agent.includes(k))?.[1] ?? '';
	const os =
		[
			['iPhone', 'iOS'],
			['iPad', 'iPadOS'],
			['Android', 'Android'],
			['Mac OS X', 'macOS'],
			['Windows', 'Windows'],
			['CrOS', 'ChromeOS'],
			['Linux', 'Linux']
		].find(([k]) => agent.includes(k))?.[1] ?? '';
	if (browser && os) return t('device.on', { browser, os });
	return browser || os || t('device.unknown');
}
