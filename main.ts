// The entry <-> env lifecycle is now owned by the static Go core in core/core.
// Node stays only as the required entrypoint: it launches the vendored binary,
// forwards the environment, and mirrors the child's exit status.

import { spawn } from 'node:child_process';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const coreBinary = join(here, 'core', process.platform === 'win32' ? 'core.exe' : 'core');
const core = spawn(coreBinary, [], { stdio: 'inherit', env: process.env });

let keepAlive: NodeJS.Timeout;
core.on('exit', (code, signal) => {
  clearInterval(keepAlive);
  process.exit(signal ? 1 : (code ?? 0));
});
core.on('error', (err) => {
  process.stderr.write(`[entry] failed to launch core: ${err.message}\n`);
  clearInterval(keepAlive);
  process.exit(1);
});
keepAlive = setInterval(() => {}, 1 << 30);
