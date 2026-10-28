// Writes .br, .zst and .gz siblings beside every built _app/ file whose bytes do not hold the
// base placeholder. A placeholder-bearing file gets none: the server rewrites it at startup and
// serves only the rewritten identity bytes, so no response in any encoding carries the placeholder.
import { existsSync, readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { brotliCompressSync, constants } from 'node:zlib';
import { createHash } from 'node:crypto';

const placeholder = '__boxset_base__';
const dist = join(import.meta.dir, '..', '..', 'internal', 'server', 'dist');
const app = join(dist, '_app');

if (!existsSync(join(dist, '200.html')) || !existsSync(app)) {
	throw new Error(`precompress: ${dist} holds no built shell; run the build first`);
}

// OFL clause 2 requires the original copyright and licence beside the redistributed font.
const fontPackage = join(import.meta.dir, '..', 'node_modules', '@fontsource-variable', 'archivo');
const font = readFileSync(join(fontPackage, 'files', 'archivo-latin-standard-normal.woff2'));
const assets = join(app, 'immutable', 'assets');
const fonts = readdirSync(assets).filter((name) => name.endsWith('.woff2'));
if (fonts.length !== 1 || !readFileSync(join(assets, fonts[0])).equals(font)) {
	throw new Error('precompress: expected the single unchanged Archivo Latin full-axis font');
}
const notice = readFileSync(join(fontPackage, 'LICENSE'));
const noticeHash = createHash('sha256').update(notice).digest('hex');
writeFileSync(join(assets, `archivo-OFL.${noticeHash.slice(0, 16)}.txt`), notice);

const compressed = /\.(br|zst|gz)$/;
let written = 0;
let skipped = 0;

for (const entry of readdirSync(app, { recursive: true, withFileTypes: true })) {
	if (!entry.isFile() || compressed.test(entry.name)) continue;
	const file = join(entry.parentPath, entry.name);
	const bytes = readFileSync(file);
	if (bytes.includes(placeholder)) {
		skipped++;
		continue;
	}
	writeFileSync(
		`${file}.br`,
		brotliCompressSync(bytes, { params: { [constants.BROTLI_PARAM_QUALITY]: 11 } })
	);
	writeFileSync(`${file}.zst`, Bun.zstdCompressSync(bytes, { level: 19 }));
	writeFileSync(`${file}.gz`, Bun.gzipSync(bytes, { level: 9 }));
	written++;
}

console.log(`precompress: ${written} files compressed, ${skipped} hold the placeholder`);
