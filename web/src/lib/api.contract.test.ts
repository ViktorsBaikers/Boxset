// Type-level contract between the generated API types and api.ts, checked by svelte-check in
// `bun --cwd=web run check:api`. A drift that breaks one of these lines fails that check.
import type { components } from './api.gen';
import type { setup, Session, User } from './api';

type Equals<A, B> =
	(<T>() => T extends A ? 1 : 2) extends <T>() => T extends B ? 1 : 2 ? true : false;
type Schemas = components['schemas'];

// @ts-expect-error A user never carries a password.
export type UserPassword = User['password'];
// @ts-expect-error A problem never echoes an input value.
export type ProblemValue = Schemas['Problem']['value'];

export const contract: [
	Equals<Schemas['Problem']['code'], string>,
	Equals<User['createdAt'], string>,
	Equals<User['role'], 'admin' | 'member' | 'viewer'>,
	Equals<Awaited<ReturnType<typeof setup>>, Schemas['User']>,
	Equals<Parameters<typeof setup>[1], Schemas['Credentials']>,
	// The direct-peer grant answers without a user, and the warning and logout
	// facts are always serialized, so the SPA reads them off generated types.
	Equals<Session['user'], Schemas['User'] | undefined>,
	Equals<Session['credential'], 'cookie' | 'key' | 'local_bypass'>,
	Equals<Session['localBypassEnabled'], boolean>,
	Equals<Session['canLogout'], boolean>
] = [true, true, true, true, true, true, true, true, true];
