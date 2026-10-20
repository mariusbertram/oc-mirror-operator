import { spawnSync } from 'node:child_process';
import compatibility from '../console-compatibility.json' with { type: 'json' };

const consoleVersion = process.env.CONSOLE_VERSION || '4.19';
const profile = compatibility[consoleVersion];
if (!profile) {
  throw new Error(`Unsupported console version ${consoleVersion}`);
}

const bundler = profile.bundler === 'rspack' ? 'rspack' : 'webpack';
const args = bundler === 'rspack'
  ? ['build', '-c', 'rspack.plugin.js']
  : ['--config', 'webpack.plugin.js', '--mode', 'production'];

const result = spawnSync(bundler, args, {
  cwd: new URL('..', import.meta.url),
  stdio: 'inherit',
  env: { ...process.env, NODE_ENV: 'production' },
  shell: process.platform === 'win32',
});
if (result.error) throw result.error;
process.exit(result.status ?? 1);
