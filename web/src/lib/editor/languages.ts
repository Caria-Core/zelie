import type { Extension } from '@codemirror/state';

// Each language is its own chunk, fetched when a file of that kind is opened.
type Loader = () => Promise<Extension>;

const legacy = async (define: () => Promise<Parameters<typeof import('@codemirror/language').StreamLanguage.define>[0]>) => {
	const { StreamLanguage } = await import('@codemirror/language');
	return StreamLanguage.define(await define());
};

const loaders: Record<string, Loader> = {
	json: () => import('@codemirror/lang-json').then((m) => m.json()),
	javascript: () => import('@codemirror/lang-javascript').then((m) => m.javascript()),
	typescript: () => import('@codemirror/lang-javascript').then((m) => m.javascript({ typescript: true })),
	cpp: () => import('@codemirror/lang-cpp').then((m) => m.cpp()),
	java: () => import('@codemirror/lang-java').then((m) => m.java()),
	python: () => import('@codemirror/lang-python').then((m) => m.python()),
	yaml: () => import('@codemirror/lang-yaml').then((m) => m.yaml()),
	xml: () => import('@codemirror/lang-xml').then((m) => m.xml()),
	html: () => import('@codemirror/lang-html').then((m) => m.html()),
	css: () => import('@codemirror/lang-css').then((m) => m.css()),
	markdown: () => import('@codemirror/lang-markdown').then((m) => m.markdown()),
	php: () => import('@codemirror/lang-php').then((m) => m.php()),
	sql: () => import('@codemirror/lang-sql').then((m) => m.sql()),
	csharp: () => legacy(() => import('@codemirror/legacy-modes/mode/clike').then((m) => m.csharp)),
	shell: () => legacy(() => import('@codemirror/legacy-modes/mode/shell').then((m) => m.shell)),
	toml: () => legacy(() => import('@codemirror/legacy-modes/mode/toml').then((m) => m.toml)),
	properties: () => legacy(() => import('@codemirror/legacy-modes/mode/properties').then((m) => m.properties)),
	lua: () => legacy(() => import('@codemirror/legacy-modes/mode/lua').then((m) => m.lua)),
	nginx: () => legacy(() => import('@codemirror/legacy-modes/mode/nginx').then((m) => m.nginx))
};

const byExtension: Record<string, string> = {
	cs: 'csharp',
	js: 'javascript',
	mjs: 'javascript',
	cjs: 'javascript',
	jsx: 'javascript',
	ts: 'typescript',
	mts: 'typescript',
	cts: 'typescript',
	tsx: 'typescript',
	c: 'cpp',
	cc: 'cpp',
	cpp: 'cpp',
	cxx: 'cpp',
	h: 'cpp',
	hh: 'cpp',
	hpp: 'cpp',
	json: 'json',
	json5: 'json',
	mcmeta: 'json',
	yml: 'yaml',
	yaml: 'yaml',
	xml: 'xml',
	html: 'html',
	htm: 'html',
	css: 'css',
	md: 'markdown',
	py: 'python',
	java: 'java',
	php: 'php',
	sql: 'sql',
	sh: 'shell',
	bash: 'shell',
	toml: 'toml',
	properties: 'properties',
	cfg: 'properties',
	ini: 'properties',
	conf: 'properties',
	lua: 'lua'
};

const byName: Record<string, string> = { 'nginx.conf': 'nginx', '.env': 'properties' };

// languageOf names the language of a file, or '' for plain text.
export function languageOf(path: string): string {
	const file = (path.split('/').pop() ?? '').toLowerCase();
	if (byName[file]) return byName[file];
	const dot = file.lastIndexOf('.');
	return dot < 0 ? '' : (byExtension[file.slice(dot + 1)] ?? '');
}

export async function loadLanguage(name: string): Promise<Extension> {
	return loaders[name]?.() ?? [];
}

// Two spaces is the habit of these; the rest use four.
export const narrowIndent = new Set(['json', 'yaml', 'javascript', 'typescript', 'xml', 'html', 'css', 'markdown', 'lua']);
