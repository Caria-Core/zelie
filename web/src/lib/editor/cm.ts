import { closeBrackets, closeBracketsKeymap } from '@codemirror/autocomplete';
import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands';
import { bracketMatching, HighlightStyle, indentOnInput, indentUnit, syntaxHighlighting } from '@codemirror/language';
import { type Diagnostic, linter, lintGutter, lintKeymap } from '@codemirror/lint';
import { highlightSelectionMatches, searchKeymap } from '@codemirror/search';
import { EditorState, type Extension, Prec } from '@codemirror/state';
import { drawSelection, EditorView, highlightActiveLine, highlightActiveLineGutter, keymap, lineNumbers } from '@codemirror/view';
import { tags as t } from '@lezer/highlight';
import { jsonProblem } from './json';
import { languageOf, loadLanguage, narrowIndent } from './languages';

// Colours come from variables set in Editor.svelte, so the light and dark
// themes need no second theme here.
const theme = EditorView.theme({
	'&': { height: '100%', color: 'var(--fg)', backgroundColor: 'var(--bg)', fontSize: '14px' },
	'&.cm-focused': { outline: 'none' },
	'.cm-scroller': { fontFamily: 'var(--font-mono, ui-monospace, SFMono-Regular, Menlo, monospace)', lineHeight: '1.6', overflow: 'auto' },
	'.cm-content': { padding: '12px 0', caretColor: 'var(--fg)' },
	'.cm-line': { padding: '0 14px 0 8px' },
	'.cm-gutters': { backgroundColor: 'var(--bg)', color: 'var(--muted)', border: 'none', paddingLeft: '4px' },
	'.cm-lineNumbers .cm-gutterElement': { padding: '0 8px 0 4px', minWidth: '2.2em' },
	'.cm-activeLine': { backgroundColor: 'var(--hover)' },
	'.cm-activeLineGutter': { backgroundColor: 'var(--hover)', color: 'var(--fg)' },
	'.cm-cursor, .cm-dropCursor': { borderLeftColor: 'var(--fg)' },
	'&.cm-focused > .cm-scroller > .cm-selectionLayer .cm-selectionBackground, .cm-selectionBackground, .cm-content ::selection': { backgroundColor: 'var(--cm-selection)' },
	'.cm-selectionMatch': { backgroundColor: 'var(--cm-match)' },
	'&.cm-focused .cm-matchingBracket': { backgroundColor: 'var(--cm-match)', outline: '1px solid var(--muted)' },
	'&.cm-focused .cm-nonmatchingBracket': { backgroundColor: 'var(--cm-error-bg)' },
	'.cm-panels': { backgroundColor: 'var(--panel)', color: 'var(--fg)', borderColor: 'var(--line)' },
	'.cm-panels.cm-panels-top': { borderBottom: '1px solid var(--line)' },
	'.cm-panel.cm-search': { padding: '8px 36px 8px 10px', display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: '6px' },
	'.cm-panel.cm-search br': { display: 'none' },
	'.cm-panel.cm-search input, .cm-panel.cm-search button': { fontSize: '13px', margin: '0' },
	'.cm-panel.cm-search input.cm-textfield': { border: '1px solid var(--line)', backgroundColor: 'var(--bg)', color: 'var(--fg)', borderRadius: '8px', padding: '3px 8px', outline: 'none', width: '9em' },
	'.cm-panel.cm-search input.cm-textfield:focus': { borderColor: 'var(--muted)' },
	'.cm-panel.cm-search button.cm-button': { backgroundImage: 'none', backgroundColor: 'var(--selected)', color: 'var(--fg)', border: 'none', borderRadius: '8px', padding: '3px 10px', cursor: 'pointer' },
	'.cm-panel.cm-search label': { fontSize: '13px', color: 'var(--muted)', display: 'inline-flex', alignItems: 'center', gap: '4px' },
	'.cm-panel.cm-search [name=close]': { color: 'var(--muted)', top: '6px', right: '8px', fontSize: '18px' },
	'.cm-searchMatch': { backgroundColor: 'var(--cm-match)', outline: 'none' },
	'.cm-searchMatch.cm-searchMatch-selected': { backgroundColor: 'var(--cm-selection)' },
	'.cm-tooltip': { backgroundColor: 'var(--bg)', color: 'var(--fg)', border: '1px solid var(--line)', borderRadius: '10px', boxShadow: '0 4px 16px rgb(0 0 0 / 0.12)' },
	'.cm-tooltip-lint': { padding: '2px 0' },
	'.cm-diagnostic': { padding: '4px 10px', fontSize: '13px', borderLeft: '3px solid var(--danger)', marginLeft: '0' },
	'.cm-diagnostic-error': { borderLeftColor: 'var(--danger)' },
	'.cm-lintRange-error': { backgroundImage: 'none', textDecoration: 'underline wavy var(--danger)', textUnderlineOffset: '3px' },
	'.cm-lint-marker-error': { content: 'none', width: '0.9em', height: '0.9em', backgroundColor: 'var(--danger)', borderRadius: '50%' },
	'.cm-gutter-lint': { width: '1em' }
});

const highlight = HighlightStyle.define([
	{ tag: [t.keyword, t.modifier, t.controlKeyword, t.operatorKeyword], color: 'var(--cm-keyword)' },
	{ tag: [t.string, t.special(t.string), t.regexp], color: 'var(--cm-string)' },
	{ tag: [t.number, t.bool, t.null, t.atom, t.color], color: 'var(--cm-number)' },
	{ tag: [t.comment, t.lineComment, t.blockComment, t.docComment], color: 'var(--cm-comment)', fontStyle: 'italic' },
	{ tag: [t.function(t.variableName), t.function(t.propertyName), t.propertyName, t.attributeName], color: 'var(--cm-property)' },
	{ tag: [t.typeName, t.className, t.namespace, t.definition(t.typeName)], color: 'var(--cm-type)' },
	{ tag: [t.tagName, t.heading, t.link], color: 'var(--cm-tag)' },
	{ tag: [t.operator, t.punctuation, t.bracket, t.separator], color: 'var(--cm-punct)' },
	{ tag: [t.meta, t.processingInstruction, t.annotation], color: 'var(--cm-comment)' },
	{ tag: t.heading, fontWeight: '600' },
	{ tag: t.strong, fontWeight: '600' },
	{ tag: t.emphasis, fontStyle: 'italic' },
	{ tag: t.invalid, color: 'var(--danger)' }
]);

export type Handle = {
	view: EditorView;
	// set replaces the whole text, keeping nothing of the history.
	set: (text: string) => void;
	reveal: (line: number, column: number) => void;
	destroy: () => void;
};

export type Options = {
	// The editor lives in this shadow root. Its styles are added to the root
	// they are used in, and in a shadow root that is done with a constructed
	// style sheet, which the panel's content security policy allows; a style
	// element in the page it does not.
	root: ShadowRoot;
	parent: HTMLElement;
	path: string;
	text: string;
	label: string;
	onchange: (text: string) => void;
	onsave: () => void;
	// Told what is wrong with the text, or null when nothing is.
	onproblem: (problem: { line: number; column: number; message: string } | null) => void;
};

export async function createEditor(o: Options): Promise<Handle> {
	const name = languageOf(o.path);
	const lang = await loadLanguage(name);
	const lintsJson = /\.(json|mcmeta)$/i.test(o.path);

	const extensions: Extension[] = [
		lineNumbers(),
		highlightActiveLineGutter(),
		highlightActiveLine(),
		history(),
		drawSelection(),
		indentOnInput(),
		bracketMatching(),
		closeBrackets(),
		highlightSelectionMatches(),
		EditorState.tabSize.of(narrowIndent.has(name) ? 2 : 4),
		indentUnit.of(narrowIndent.has(name) ? '  ' : '    '),
		syntaxHighlighting(highlight),
		theme,
		lang,
		Prec.highest(
			keymap.of([
				{
					key: 'Mod-s',
					preventDefault: true,
					run: () => {
						o.onsave();
						return true;
					}
				}
			])
		),
		keymap.of([...closeBracketsKeymap, ...defaultKeymap, ...searchKeymap, ...historyKeymap, ...lintKeymap, indentWithTab]),
		EditorView.contentAttributes.of({ 'aria-label': o.label, spellcheck: 'false', autocorrect: 'off', autocapitalize: 'off' }),
		EditorView.updateListener.of((u) => {
			if (u.docChanged) o.onchange(u.state.doc.toString());
		})
	];

	if (lintsJson) {
		extensions.push(
			lintGutter(),
			linter(
				(view): Diagnostic[] => {
					const p = jsonProblem(view.state.doc.toString());
					o.onproblem(p && { line: p.line, column: p.column, message: p.message });
					if (!p) return [];
					const line = view.state.doc.lineAt(Math.min(p.pos, view.state.doc.length));
					// Underline the character the parser stopped at, or the
					// line's end when it stopped after the last one.
					const from = Math.min(p.pos, line.to);
					const to = from < line.to ? from + 1 : line.to;
					return [{ from: Math.min(from, to), to: Math.max(from, to), severity: 'error', message: p.message }];
				},
				{ delay: 250 }
			)
		);
	}

	const view = new EditorView({ root: o.root, parent: o.parent, state: EditorState.create({ doc: o.text, extensions }) });
	if (lintsJson) o.onproblem(jsonProblem(o.text));

	return {
		view,
		set(text) {
			view.setState(EditorState.create({ doc: text, extensions }));
		},
		reveal(line, column) {
			const l = view.state.doc.line(Math.min(Math.max(line, 1), view.state.doc.lines));
			const pos = Math.min(l.from + Math.max(column - 1, 0), l.to);
			view.dispatch({ selection: { anchor: pos }, effects: EditorView.scrollIntoView(pos, { y: 'center' }) });
			view.focus();
		},
		destroy: () => view.destroy()
	};
}
