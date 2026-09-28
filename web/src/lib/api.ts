import type { Msg } from './i18n';

export class ApiError extends Error {
	constructor(
		readonly status: number,
		// What the server said, to show in the user's language with say().
		readonly msg: Msg,
		// The server wants the user to confirm it is them, then try again.
		readonly confirm = false
	) {
		super(msg.text);
	}
}

// errorFrom reads an error response's body. Every error the panel sends
// has a code; a body without one (a proxy's page, say) keeps its status.
export function errorFrom(status: number, statusText: string, data: Record<string, unknown>): ApiError {
	const text = typeof data.error === 'string' ? data.error : statusText;
	const code = typeof data.code === 'string' ? data.code : 'other.detail';
	const params = (data.params as Record<string, unknown>) ?? (code === 'other.detail' ? { detail: text } : undefined);
	return new ApiError(status, { code, params, text }, data.confirm === true);
}

// api calls the panel's JSON API. Errors carry the server's message.
export async function api<T = unknown>(method: string, path: string, body?: unknown): Promise<T> {
	const res = await fetch('/api' + path, {
		method,
		headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
		body: body === undefined ? undefined : typeof body === 'string' ? body : JSON.stringify(body)
	});
	if (res.status === 204) return undefined as T;
	const data = await res.json().catch(() => ({}));
	if (!res.ok) throw errorFrom(res.status, res.statusText, data);
	return data as T;
}

export type Me = {
	logged_in: boolean;
	email?: string;
	admin?: boolean;
	verified?: boolean;
	enroll?: boolean;
	methods?: ('passkey' | 'totp' | 'recovery')[];
};
