import { api, errorFrom } from './api';

export type FileEntry = {
	name: string;
	size: number;
	mode: string;
	modified: string;
	dir: boolean;
	symlink: boolean;
	target?: string;
};

export type FileList = { path: string; entries: FileEntry[]; truncated?: boolean };

// The editor opens up to this many bytes, as the core does.
export const editLimit = 4 << 20;

const base = (id: string) => `/games/${encodeURIComponent(id)}/files`;
const query = (path: string, more = '') => `?path=${encodeURIComponent(path)}${more}`;

// join puts a name below a folder. The top folder is the empty string.
export function join(dir: string, name: string): string {
	return dir ? `${dir}/${name}` : name;
}

export function parent(path: string): string {
	const i = path.lastIndexOf('/');
	return i < 0 ? '' : path.slice(0, i);
}

// clean drops the slashes at the ends, which the address may have.
export function clean(path: string): string {
	return path.replace(/^\/+|\/+$/g, '');
}

const archive = /\.(zip|tar|tar\.gz|tgz)$/i;
export const isArchive = (name: string) => archive.test(name);

const when = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' });
export const modified = (iso: string) => when.format(new Date(iso));

export const list = (id: string, path: string) => api<FileList>('GET', `${base(id)}/list${query(path)}`);

export async function read(id: string, path: string): Promise<string> {
	const res = await fetch(`/api${base(id)}/content${query(path)}`);
	if (!res.ok) throw errorFrom(res.status, res.statusText, await res.json().catch(() => ({})));
	return res.text();
}

// write saves a file's text. With create it makes a new file and fails when
// the name is taken.
export async function write(id: string, path: string, text: string, create = false): Promise<void> {
	const res = await fetch(`/api${base(id)}/content${query(path, create ? '&new=1' : '')}`, {
		method: 'PUT',
		headers: { 'Content-Type': 'text/plain; charset=utf-8' },
		body: text
	});
	if (!res.ok) throw errorFrom(res.status, res.statusText, await res.json().catch(() => ({})));
}

export const makeFolder = (id: string, path: string) => api('POST', `${base(id)}/mkdir`, { path });
export const rename = (id: string, from: string, to: string) => api('POST', `${base(id)}/rename`, { from, to });
export const remove = (id: string, paths: string[]) => api('POST', `${base(id)}/delete`, { paths });
export const compress = (id: string, dir: string, paths: string[]) => api<{ name: string }>('POST', `${base(id)}/compress`, { dir, paths });
export const extract = (id: string, path: string, dir = '') => api<{ entries: number; bytes: number }>('POST', `${base(id)}/extract`, { path, dir });

// downloadUrl is where the browser fetches a file from, which saves it.
export const downloadUrl = (id: string, path: string) => `/api${base(id)}/download${query(path)}`;

export type Upload = { promise: Promise<void>; abort: () => void };

// upload streams a file to the server. fetch cannot report how much has
// gone, so this is a request of the old kind.
export function upload(id: string, path: string, file: File, progress: (sent: number) => void): Upload {
	const xhr = new XMLHttpRequest();
	const promise = new Promise<void>((resolve, reject) => {
		xhr.open('PUT', `/api${base(id)}/upload${query(path)}`);
		xhr.setRequestHeader('Content-Type', 'application/octet-stream');
		xhr.upload.onprogress = (e) => progress(e.loaded);
		xhr.onload = () => {
			if (xhr.status >= 200 && xhr.status < 300) return resolve();
			let data: Record<string, unknown> = {};
			try {
				data = JSON.parse(xhr.responseText);
			} catch {
				// A page from something in front of the panel: the status is all there is.
			}
			reject(errorFrom(xhr.status, xhr.statusText, data));
		};
		xhr.onerror = () => reject(new Error('network'));
		xhr.onabort = () => reject(new DOMException('aborted', 'AbortError'));
		xhr.send(file);
	});
	return { promise, abort: () => xhr.abort() };
}
