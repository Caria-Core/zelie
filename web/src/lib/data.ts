// What the data viewer reads. The server makes every query; the page only
// says which table, filters and page.

export type Column = { name: string; type: string; nullable: boolean; key?: boolean; binary?: boolean };

export type Table = {
	schema: string;
	name: string;
	view?: boolean;
	// The database's estimate; -1 when it has none yet.
	rows: number;
	bytes: number;
	columns: Column[];
};

export type Op = 'eq' | 'ne' | 'contains' | 'starts' | 'gt' | 'lt' | 'null' | 'notnull';
export const ops: Op[] = ['contains', 'eq', 'ne', 'starts', 'gt', 'lt', 'null', 'notnull'];
// These take no value.
export const bare: Op[] = ['null', 'notnull'];

export type Filter = { column: string; op: Op; value?: string };

export type Query = {
	schema: string;
	table: string;
	filters?: Filter[];
	search?: string;
	sort?: string;
	desc?: boolean;
	offset?: number;
	full?: boolean;
};

export type Page = {
	columns: string[];
	rows: (string | null)[][];
	// Values cut short, as row and column.
	cut?: [number, number][];
	more: boolean;
};

export type Key = { id: string; name: string; binary?: boolean; type: string; ttl: number };
export type KeyPage = { keys: Key[]; cursor: string };
export type Value = {
	key: Key;
	size: number;
	columns: string[];
	rows: string[][];
	binary?: [number, number][];
	cut?: [number, number][];
};

export const has = (cells: [number, number][] | undefined, r: number, c: number) => !!cells?.some(([a, b]) => a === r && b === c);

const compact = new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 });
export const approx = (n: number) => compact.format(n);
