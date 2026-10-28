// Live-reload supervisor of the private dev loop (compose.dev.yaml, service `dev`), run by Bun
// as uid 1000 with a read-only root filesystem: it writes only named volumes and /tmp.
//
// 1. Preflight, before either child starts. The bind-mounted manifests must equal the stamps the
//    `live` image was built from (/usr/local/share/boxset/deps/{web,go}.sha256), else the image is
//    stale and the service exits `deps_image_stale ecosystem=web|go` without writing anything
//    (remedy: `make dev`). A web/node_modules volume whose stamp differs from the image's is
//    replaced from the read-only seed, the stamp written last (`DEV-DEPS web=replaced`; equal:
//    `web=current`); any error exits `deps_reconcile_failed path=web/node_modules`. Compose never
//    refreshes a populated named volume by itself. The Go module cache, /config and the other
//    volumes are never reconciled: Go fills missing modules only from the seed file proxy
//    (GOPROXY=file:///usr/local/share/boxset/deps/go/cache/download, -mod=readonly).
// 2. Vite dev server for web/ on all container interfaces (the host publish is loopback-only).
// 3. The backend: `go build ./cmd/boxset`, run, and rebuild/restart on a Go, SQL or module change.
//    This never runs the web `build` script (it rewrites the embedded shell directory).
import { createHash } from 'node:crypto';
import {
	copyFileSync,
	mkdirSync,
	readFileSync,
	readdirSync,
	renameSync,
	rmSync,
	symlinkSync,
	watch,
	writeFileSync
} from 'node:fs';
import { join } from 'node:path';

const root = '/src';
const web = join(root, 'web');
const seed = '/usr/local/share/boxset/deps';
const binary = '/tmp/boxset';

const digest = (bytes: Uint8Array) => createHash('sha256').update(bytes).digest('hex');
const sum = (file: string) => digest(readFileSync(file));

// The stamp formats are the `live` stage's: `bun --version` then `sha256sum package.json bun.lock`,
// and `sha256sum go.mod go.sum`.
const current = {
	web: () =>
		`${Bun.version}\n${sum(join(web, 'package.json'))}  package.json\n${sum(join(web, 'bun.lock'))}  bun.lock\n`,
	go: () => `${sum(join(root, 'go.mod'))}  go.mod\n${sum(join(root, 'go.sum'))}  go.sum\n`
};

function die(word: string, detail: string, error?: unknown): never {
	console.error(`${word} ${detail}`);
	if (error !== undefined) console.error(String(error));
	process.exit(1);
}

function preflight() {
	const stale: string[] = [];
	for (const ecosystem of ['web', 'go'] as const) {
		let imageStamp: string;
		try {
			imageStamp = readFileSync(join(seed, `${ecosystem}.sha256`), 'utf8');
		} catch (error) {
			die('deps_reconcile_failed', `path=${seed}/${ecosystem}.sha256`, error);
		}
		if (current[ecosystem]() !== imageStamp) stale.push(ecosystem);
	}
	if (stale.length > 0) {
		for (const ecosystem of stale) console.error(`deps_image_stale ecosystem=${ecosystem}`);
		process.exit(1);
	}

	const modules = join(web, 'node_modules');
	const stamp = join(modules, '.boxset-deps.sha256');
	const want = readFileSync(join(seed, 'web.sha256'), 'utf8');
	let have = '';
	try {
		have = readFileSync(stamp, 'utf8');
	} catch {
		// no stamp: an unstamped volume is replaced
	}
	if (have === want) {
		console.log('DEV-DEPS web=current');
		return;
	}
	try {
		// The stamp goes first so an interrupted replacement is never mistaken for a current one.
		// rmSync removes a link, never what it points to; cp -P copies links as links.
		rmSync(stamp, { force: true });
		for (const entry of readdirSync(modules))
			rmSync(join(modules, entry), { recursive: true, force: true });
		const copy = Bun.spawnSync(['cp', '-RP', '--', `${seed}/web/node_modules/.`, `${modules}/`]);
		if (copy.exitCode !== 0)
			throw new Error(`cp exited ${copy.exitCode}: ${copy.stderr.toString()}`);
		writeFileSync(stamp, want);
	} catch (error) {
		die('deps_reconcile_failed', 'path=web/node_modules', error);
	}
	console.log('DEV-DEPS web=replaced');
}

preflight();

// Loading the inlang project writes beside its settings, so Vite gets a writable copy under /tmp
// (web/vite.config.ts reads BOXSET_I18N_DIR); the message files stay the bind-mounted ones.
const i18n = '/tmp/i18n';
rmSync(i18n, { recursive: true, force: true });
mkdirSync(join(i18n, 'project.inlang'), { recursive: true });
copyFileSync(
	join(web, 'project.inlang', 'settings.json'),
	join(i18n, 'project.inlang', 'settings.json')
);
symlinkSync(join(web, 'messages'), join(i18n, 'messages'));
symlinkSync(join(web, 'node_modules'), join(i18n, 'node_modules'));

let backend: Bun.Subprocess | null = null;
let generation = 0;
let stopping = false;
let restartingVite = false;
let viteAgain = false;

// A child started for a config change may exit on a broken intermediate save; the backend keeps
// serving and the next vite.config.ts event starts another child. Only the first child's exit ends
// the service.
function spawnVite(restarted = false) {
	const child = Bun.spawn(
		[
			'bun',
			'--no-install',
			'/src/web/node_modules/vite/bin/vite.js',
			'dev',
			'--configLoader',
			'native',
			'--host',
			'0.0.0.0',
			'--port',
			'5173',
			'--strictPort'
		],
		{
			cwd: web,
			stdout: 'inherit',
			stderr: 'inherit',
			env: { ...process.env, BOXSET_I18N_DIR: i18n }
		}
	);
	void child.exited.then((code) => {
		if (vite !== child || stopping || restartingVite) return;
		if (restarted) {
			console.error(`DEV-VITE exited code=${code}; waiting for the next vite.config.ts change`);
			return;
		}
		console.error(`DEV-VITE exited code=${code}`);
		void shutdown(code === 0 ? 1 : code);
	});
	return child;
}
let vite = spawnVite();

// Bun's native config module stays cached across Vite's in-process restart.
// Restart the child for config changes only; component changes remain Vite HMR.
async function restartVite() {
	if (stopping) return;
	if (restartingVite) {
		viteAgain = true;
		return;
	}
	restartingVite = true;
	try {
		do {
			viteAgain = false;
			const old = vite;
			old.kill('SIGTERM');
			const killer = setTimeout(() => old.kill('SIGKILL'), 35_000);
			await old.exited;
			clearTimeout(killer);
			if (!stopping) vite = spawnVite(true);
		} while (viteAgain && !stopping);
	} finally {
		restartingVite = false;
	}
}
let viteTimer: NodeJS.Timeout | undefined;
watch('/checkout/source/web', (_event, file) => {
	if (file !== 'vite.config.ts') return;
	clearTimeout(viteTimer);
	viteTimer = setTimeout(() => void restartVite(), 150);
});

async function stopBackend() {
	const old = backend;
	if (old === null) return;
	backend = null;
	old.kill('SIGTERM');
	const killer = setTimeout(() => old.kill('SIGKILL'), 35_000);
	await old.exited;
	clearTimeout(killer);
}

async function buildAndRestart() {
	const build = Bun.spawn(['go', 'build', '-o', `${binary}.next`, './cmd/boxset'], {
		cwd: root,
		stdout: 'inherit',
		stderr: 'inherit'
	});
	if ((await build.exited) !== 0) {
		console.error(
			'DEV-BUILD failed; the running backend, if any, keeps serving the last good build'
		);
		return;
	}
	renameSync(`${binary}.next`, binary);
	await stopBackend();
	if (stopping) return;
	generation++;
	const id = `${generation}-${digest(readFileSync(binary)).slice(0, 12)}`;
	const child = Bun.spawn([binary, 'serve'], { cwd: root, stdout: 'inherit', stderr: 'inherit' });
	backend = child;
	console.log(`DEV-BACKEND build=${id} pid=${child.pid}`);
	void child.exited.then((code) => {
		if (backend === child) {
			backend = null;
			console.error(`DEV-BACKEND exited code=${code}`);
		}
	});
}

// One build at a time; a change during a build schedules exactly one more.
let building = false;
let again = false;
async function rebuild() {
	if (building) {
		again = true;
		return;
	}
	building = true;
	try {
		do {
			again = false;
			await buildAndRestart();
		} while (again && !stopping);
	} finally {
		building = false;
	}
}

let timer: NodeJS.Timeout | undefined;
function changed(file: string | null) {
	if (file === null || file.startsWith('server/dist/') || file.startsWith('internal/server/dist/'))
		return;
	if (!/(\.go|\.sql|go\.mod|go\.sum)$/.test(file)) return;
	clearTimeout(timer);
	timer = setTimeout(() => void rebuild(), 150);
}
for (const dir of ['cmd', 'internal']) {
	watch(join(root, dir), { recursive: true }, (_event, file) =>
		changed(file === null ? null : `${dir}/${file}`)
	);
}
for (const file of ['go.mod', 'go.sum']) watch(join(root, file), () => changed(file));

async function shutdown(code: number) {
	if (stopping) return;
	stopping = true;
	vite.kill('SIGTERM');
	await stopBackend();
	await vite.exited;
	process.exit(code);
}
process.on('SIGTERM', () => void shutdown(0));
process.on('SIGINT', () => void shutdown(0));

await rebuild();
