import { ApiError } from './api';
import { say, t } from './i18n';

// Cancelled is thrown when the user backs out of something, which needs no
// message.
export class Cancelled extends Error {}

// messageOf turns anything thrown into text for the user: the panel's own
// message in the user's language, or a generic line for anything else.
export function messageOf(e: unknown): string {
	if (e instanceof ApiError) return say(e.msg);
	if (e instanceof Cancelled) return '';
	if (e instanceof DOMException && e.name === 'NotAllowedError') return '';
	return t('common.error');
}
