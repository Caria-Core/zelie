import { ApiError } from './api';
import { t } from './i18n';

// messageOf turns anything thrown into text for the user. Messages from the
// panel's API are written for people; anything else gets a generic line.
export function messageOf(e: unknown): string {
	if (e instanceof ApiError) return e.message;
	if (e instanceof DOMException && e.name === 'NotAllowedError') return '';
	return t('common.error');
}
