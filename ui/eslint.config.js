// Flat config (ESLint v9). See https://eslint.org/docs/latest/use/configure/configuration-files
const js = require('@eslint/js');
const tseslint = require('typescript-eslint');
const reactHooks = require('eslint-plugin-react-hooks');
const globals = require('globals');

module.exports = tseslint.config(
  {
    // Build output and vendored/generated files.
    ignores: ['dist/**', 'node_modules/**', 'src/plugin/plugin-manifest.json'],
  },

  js.configs.recommended,
  ...tseslint.configs.recommended,

  {
    files: ['src/**/*.{ts,tsx}'],
    languageOptions: {
      globals: { ...globals.browser },
    },
    plugins: {
      'react-hooks': reactHooks,
    },
    rules: {
      // Only the two classic hook-correctness rules — eslint-plugin-react-hooks
      // v7's "recommended" preset bundles the new React Compiler rule family
      // (set-state-in-effect, purity, immutability, static-components, ...),
      // which assumes a codebase opting into the compiler. This is a React 17
      // app that doesn't, and those rules flag the standard
      // "fetch in useEffect, setState in the .then()" pattern used throughout
      // — not a bug here, so they'd just be noise.
      'react-hooks/rules-of-hooks': 'error',
      'react-hooks/exhaustive-deps': 'warn',

      // Flags console-plugin-only exports and route params destructured from
      // useParams (always present at runtime, typed as optional by the
      // router) — noisy without adding value here.
      '@typescript-eslint/no-unused-vars': ['warn', { argsIgnorePattern: '^_', varsIgnorePattern: '^_' }],
    },
  },

  {
    files: ['src/**/*.js'],
    languageOptions: {
      globals: { ...globals.browser },
    },
  },

  {
    // Build-time/dev-server files that happen to live under src/ but run
    // under Node (webpack.dev.js requires them directly), not the browser.
    files: ['src/dev/mocks.js', 'src/plugin/extensions.js'],
    languageOptions: {
      sourceType: 'commonjs',
      globals: { ...globals.node },
    },
  },
);
