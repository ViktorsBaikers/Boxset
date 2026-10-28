// Consumer side of the generated API contract (`bun --cwd=web run check:api`):
//   1. openapi-typescript generates the types from ../internal/api/openapi.json twice; both runs
//      must be byte-identical and equal the committed src/lib/api.gen.ts (`--write` regenerates it);
//   2. date-time fields stay strings (no Date transform);
//   3. every /api/ path api.ts calls is a path of the document;
//   4. no source under src/ calls fetch( except src/lib/api.ts;
//   5. svelte-check accepts src/lib/api.contract.test.ts, the type-level contract assertions.
// Prints CHECK-API-PASS on success; every failure prints a reason and exits 1.
import { mkdtempSync, readdirSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const doc = '../internal/api/openapi.json';
const committed = 'src/lib/api.gen.ts';
const failures: string[] = [];

function run(cmd: string[]): { code: number; out: string } {
	const p = Bun.spawnSync(cmd, { stdout: 'pipe', stderr: 'pipe' });
	return { code: p.exitCode ?? 1, out: p.stdout.toString() + p.stderr.toString() };
}

function generate(out: string): string {
	const r = run(['bun', '--bun', 'x', 'openapi-typescript', doc, '-o', out]);
	if (r.code !== 0) {
		console.log(r.out.slice(-2000));
		throw new Error('openapi-typescript failed');
	}
	return readFileSync(out, 'utf8');
}

const tmp = mkdtempSync(join(tmpdir(), 'check-api-'));
try {
	const first = generate(join(tmp, 'first.ts'));
	const second = generate(join(tmp, 'second.ts'));
	if (first !== second) failures.push('generation is not deterministic: two runs differ');
	if (process.argv.includes('--write')) {
		writeFileSync(committed, first);
		console.log(`wrote ${committed}`);
	}
	if (readFileSync(committed, 'utf8') !== first) {
		failures.push(
			`${committed} differs from the types of ${doc}: run bun scripts/check-api.ts --write`
		);
	}
	if (!/createdAt: string;/.test(first) || /\bDate\b/.test(first)) {
		failures.push('date-time is no longer generated as string');
	}
} finally {
	rmSync(tmp, { recursive: true, force: true });
}

const paths = Object.keys(JSON.parse(readFileSync(doc, 'utf8')).paths);
const client = readFileSync('src/lib/api.ts', 'utf8');
const used = [...client.matchAll(/'(\/api\/[^'`]*)'/g)].map((m) => m[1]);
if (used.length === 0) failures.push('src/lib/api.ts calls no /api/ path');
for (const p of used)
	if (!paths.includes(p)) failures.push(`src/lib/api.ts calls ${p}, which the document lacks`);

function walk(dir: string): string[] {
	return readdirSync(dir).flatMap((name) => {
		const p = join(dir, name);
		return statSync(p).isDirectory() ? walk(p) : [p];
	});
}
for (const file of walk('src')) {
	if (file === join('src', 'lib', 'api.ts') || !/\.(ts|js|svelte)$/.test(file)) continue;
	if (/\bfetch\(/.test(readFileSync(file, 'utf8')))
		failures.push(`${file} calls fetch( outside src/lib/api.ts`);
}

const sync = run(['bun', '--bun', 'svelte-kit', 'sync']);
const check = run([
	'bun',
	'--bun',
	'svelte-check',
	'--tsconfig',
	'./tsconfig.json',
	'--threshold',
	'error'
]);
if (sync.code !== 0 || check.code !== 0) {
	console.log((sync.out + check.out).slice(-3000));
	failures.push('svelte-check rejects the type contract (src/lib/api.contract.test.ts)');
}

if (failures.length > 0) {
	for (const f of failures) console.log(`check-api: ${f}`);
	process.exit(1);
}
console.log(`CHECK-API-PASS paths=${paths.length} client_paths=${used.length} deterministic=true`);
