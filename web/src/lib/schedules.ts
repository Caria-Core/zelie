import type { Msg } from './i18n';

export type TaskAction = 'command' | 'power' | 'backup';
export type PowerAction = 'start' | 'stop' | 'restart' | 'kill';

export type Task = {
	action: TaskAction;
	// The console command, or the power action; empty for a backup.
	data: string;
	// Seconds to wait before the task, 0 to 900.
	delay: number;
	// Go on with the next task if this one fails.
	continue: boolean;
};

export type Schedule = {
	id: number;
	name: string;
	cron: string;
	only_running: boolean;
	enabled: boolean;
	tasks: Task[];
	next_run_at?: string;
	last_run_at?: string;
	last_state?: 'running' | 'done' | 'failed' | 'skipped';
	last_error?: Msg;
	running: boolean;
};

export type Schedules = { time_zone: string; schedules: Schedule[] };

// What the form sends: a schedule without what the server works out.
export type ScheduleForm = Pick<Schedule, 'name' | 'cron' | 'only_running' | 'enabled' | 'tasks'>;

export const presets = [
	{ id: 'hourly', cron: '0 * * * *' },
	{ id: 'sixHours', cron: '0 */6 * * *' },
	{ id: 'daily', cron: '0 4 * * *' },
	{ id: 'weekly', cron: '0 4 * * 1' }
] as const;

export type PresetId = (typeof presets)[number]['id'] | 'custom';

export function presetOf(cron: string): PresetId {
	const normal = cron.trim().split(/\s+/).join(' ');
	return presets.find((p) => p.cron === normal)?.id ?? 'custom';
}

export const powerActions: PowerAction[] = ['start', 'stop', 'restart', 'kill'];
export const maxDelay = 900;
export const maxTasks = 20;

export const emptyTask = (): Task => ({ action: 'command', data: '', delay: 0, continue: false });
