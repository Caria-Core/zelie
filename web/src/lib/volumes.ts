export type Volume = {
	id: number;
	path: string;
	limit_mb: number;
	// Null until Zelie has measured it, about once a minute.
	used_bytes: number | null;
	created_at: string;
};

// The sizes a volume's limit can be set to, up to what the disk holds.
// A limit set some other way is kept in the list.
export function sizeSteps(diskMB: number, current?: number): number[] {
	const steps = [256, 512, 1024, 2048, 5120, 10240, 20480, 51200, 102400, 204800, 512000].filter((s) => s <= diskMB);
	if (current && !steps.includes(current)) steps.push(current);
	return steps.sort((a, b) => a - b);
}

export const defaultSize = 1024;

