// vsce rewrites relative image paths for the Marketplace page but never checks
// that the file exists, so a broken image ships silently. This runs before
// packaging, where a missing screenshot is still cheap to fix.
import { existsSync } from 'node:fs';
import { readFileSync } from 'node:fs';

const readme = readFileSync('README.md', 'utf8');
const relative = [...readme.matchAll(/!\[[^\]]*\]\((?!https?:)([^)\s]+)/g)].map(
    (m) => m[1],
);
const missing = relative.filter((file) => !existsSync(file));

if (missing.length > 0) {
    console.error(
        `README.md references images that do not exist:\n  ${missing.join('\n  ')}`,
    );
    process.exit(1);
}
