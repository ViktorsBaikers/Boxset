import { lifecycle, session, status, type Session, type Status } from './api';

export type Access = { state: Status['state']; session?: Session; status?: Status };

// Liveness is public; identity and readiness are confirmed afresh before private paint.
export async function access(signal?: AbortSignal): Promise<Access> {
	const state = await lifecycle(signal);
	if (state !== 'ready') return { state };
	const current = await session(signal);
	const snapshot = await status(signal);
	return { state: snapshot.state, session: current, status: snapshot };
}
