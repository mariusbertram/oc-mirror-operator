import { spawnSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';
import compatibility from '../console-compatibility.json' with { type: 'json' };

const consoleVersion = process.env.CONSOLE_VERSION;
const profile = compatibility[consoleVersion];

if (!profile) {
  throw new Error(
    `CONSOLE_VERSION must be one of ${Object.keys(compatibility).join(', ')}, got "${consoleVersion ?? ''}"`,
  );
}

const packages = {
  '@openshift-console/dynamic-plugin-sdk': profile.sdk,
  '@openshift-console/dynamic-plugin-sdk-webpack': profile.sdkWebpack,
  '@patternfly/react-core': profile.patternflyCore,
  '@patternfly/react-icons': profile.patternflyIcons,
  '@patternfly/react-table': profile.patternflyTable,
  '@types/react': profile.reactTypes,
  '@types/react-dom': profile.reactDomTypes,
  react: profile.react,
  'react-dom': profile.react,
  'react-router': profile.router,
  'react-router-dom': profile.router,
  typescript: profile.typescript,
  webpack: profile.webpack,
  'webpack-cli': profile.webpackCli,
};
if (profile.routerTypes) packages['@types/react-router-dom'] = profile.routerTypes;

const npm = process.platform === 'win32' ? 'npm.cmd' : 'npm';
const options = {
  cwd: new URL('..', import.meta.url),
  stdio: 'inherit',
  shell: process.platform === 'win32',
};
const manifestURL = new URL('../package.json', import.meta.url);
const originalManifest = readFileSync(manifestURL, 'utf8');
const manifest = JSON.parse(originalManifest);
Object.assign(manifest.devDependencies, packages);
if (!profile.routerTypes) delete manifest.devDependencies['@types/react-router-dom'];

// Resolve the entire profile in one transaction. A subsequent npm uninstall
// would otherwise reinstall the default profile from the root manifest.
try {
  writeFileSync(manifestURL, `${JSON.stringify(manifest, null, 2)}\n`);
  const result = spawnSync(npm, ['install', '--package-lock=false'], options);
  if (result.error) throw result.error;
  if (result.status !== 0) {
    throw new Error(`Installing console ${consoleVersion} failed with exit code ${result.status}`);
  }
} finally {
  writeFileSync(manifestURL, originalManifest);
}

const installed = {};
for (const [name, expected] of Object.entries(packages)) {
  const actual = JSON.parse(readFileSync(new URL(`../node_modules/${name}/package.json`, import.meta.url), 'utf8')).version;
  if (actual !== expected) {
    throw new Error(`${name}: expected ${expected}, installed ${actual}`);
  }
  installed[name] = actual;
}
writeFileSync(new URL('../console-profile-installed.json', import.meta.url),
  `${JSON.stringify({ consoleVersion, packages: installed }, null, 2)}\n`);
writeFileSync(new URL('../tsconfig.console.json', import.meta.url), `${JSON.stringify({
  extends: './tsconfig.plugin.json',
  compilerOptions: {
    paths: {
      '@/*': ['src/*'],
      '@router': [`src/router/${profile.routerAPI}.ts`],
      '@compat': [`src/compat/${profile.patternflyCore.startsWith('5.') ? 'legacy' : 'modern'}.tsx`],
    },
  },
}, null, 2)}\n`);
