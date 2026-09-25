export class ApiError extends Error {
	constructor(
		readonly status: number,
		message: string
	) {
		super(message);
	}
}

// api calls the panel's JSON API. Errors carry the server's message, which
// is written to be shown to the user.
export async function api<T = unknown>(method: string, path: string, body?: unknown): Promise<T> {
	const res = await fetch('/api' + path, {
		method,
		headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
		body: body === undefined ? undefined : typeof body === 'string' ? body : JSON.stringify(body)
	});
	if (res.status === 204) return undefined as T;
	const data = await res.json().catch(() => ({}));
	if (!res.ok) throw new ApiError(res.status, data.error ?? res.statusText);
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
