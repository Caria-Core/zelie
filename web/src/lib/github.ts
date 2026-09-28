import { api } from './api';
import type { Msg } from './i18n';

export type GitHub = {
	connected: boolean;
	slug?: string;
	owner?: string;
	html_url?: string;
	install_url?: string;
	installations?: { id: number; account: string; all: boolean }[];
	webhook?: { state: 'ok' | 'failing' | 'none'; last?: string; status?: string };
	// The App was made for an address GitHub cannot reach, so it has no webhook.
	private?: boolean;
	error?: Msg;
};

export type Repository = { full_name: string; private: boolean; default_branch: string };

export function status(): Promise<GitHub> {
	return api<GitHub>('GET', '/github');
}

export function repositories(): Promise<Repository[]> {
	return api<Repository[]>('GET', '/github/repos');
}

// createApp sends the browser to GitHub with the panel's App manifest.
// GitHub only takes it as a form post; it comes back to /github/created.
export function createApp(action: string, manifest: string): void {
	const form = document.createElement('form');
	form.method = 'post';
	form.action = action;
	const input = document.createElement('input');
	input.type = 'hidden';
	input.name = 'manifest';
	input.value = manifest;
	form.append(input);
	document.body.append(form);
	form.submit();
}
