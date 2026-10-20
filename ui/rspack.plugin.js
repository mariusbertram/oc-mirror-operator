// rspack build for OpenShift 5.0: the 5.0 console is itself built with rspack,
// and a webpack-produced remote crashes its module-federation runtime with
// "Shared module is not available for eager consumption". Everything mirrors
// webpack.plugin.js except for the rspack-native loaders/plugins.
const path = require('path');
const { CssExtractRspackPlugin } = require('@rspack/core');
const { TsCheckerRspackPlugin } = require('ts-checker-rspack-plugin');
const { ConsoleRemotePlugin } = require('@openshift-console/dynamic-plugin-sdk-webpack');

const compatibility = require('./console-compatibility.json');
const consoleVersion = process.env.CONSOLE_VERSION || '5.0';
const compatibilityProfile = compatibility[consoleVersion];
if (!compatibilityProfile || compatibilityProfile.bundler !== 'rspack') {
  throw new Error(`rspack.plugin.js only builds rspack profiles, unsupported console version ${consoleVersion}`);
}

const pluginMetadata = require('./src/plugin/plugin-manifest.json');
pluginMetadata.dependencies['@console/pluginAPI'] = compatibilityProfile.pluginAPI;
const { extensions } = require('./console-extensions.json');

const installed = require('./console-profile-installed.json');
if (installed.consoleVersion !== consoleVersion) {
  throw new Error(`Install console profile ${consoleVersion} before building (installed ${installed.consoleVersion})`);
}
for (const [name, expected] of Object.entries(installed.packages)) {
  const actual = require(path.resolve(__dirname, 'node_modules', name, 'package.json')).version;
  if (actual !== expected) {
    throw new Error(`${name} changed after profile installation; reinstall console profile ${consoleVersion}`);
  }
}

const isProd = process.env.NODE_ENV === 'production';

module.exports = {
  mode: isProd ? 'production' : 'development',
  entry: {},
  context: path.resolve(__dirname, 'src'),
  output: {
    path: path.resolve(__dirname, 'dist/plugin'),
    publicPath: 'auto',
    filename: isProd ? '[name]-bundle-[contenthash].min.js' : '[name]-bundle.js',
    chunkFilename: isProd ? '[name]-chunk-[contenthash].min.js' : '[name]-chunk.js',
    clean: true,
  },
  resolve: {
    extensions: ['.tsx', '.ts', '.js', '.jsx'],
    alias: {
      '@': path.resolve(__dirname, 'src'),
      '@router$': path.resolve(__dirname, `src/router/${compatibilityProfile.routerAPI}.ts`),
      '@compat$': path.resolve(__dirname, `src/compat/${compatibilityProfile.patternflyCore.startsWith('5.') ? 'legacy' : 'modern'}.tsx`),
    },
  },
  module: {
    rules: [
      {
        test: /\.(jsx?|tsx?)$/,
        exclude: /[\\/]node_modules[\\/]/,
        use: {
          loader: 'builtin:swc-loader',
          options: {
            detectSyntax: 'auto',
            jsc: {
              parser: { syntax: 'typescript', tsx: true },
              transform: { react: { runtime: 'automatic' } },
              target: 'es2021',
            },
          },
        },
        type: 'javascript/auto',
      },
      {
        test: /\.css$/,
        use: [CssExtractRspackPlugin.loader, 'css-loader'],
      },
      {
        test: /\.(png|jpg|jpeg|gif|svg|woff2?|ttf|eot|otf)(\?.*$|$)/,
        type: 'asset/resource',
        generator: {
          filename: isProd ? 'assets/[contenthash][ext]' : 'assets/[name][ext]',
        },
      },
      {
        test: /\.(m?js)$/,
        resolve: { fullySpecified: false },
      },
    ],
  },
  plugins: [
    new CssExtractRspackPlugin({ filename: isProd ? 'plugin-[contenthash].css' : 'plugin.css' }),
    new ConsoleRemotePlugin({ pluginMetadata, extensions }),
    new TsCheckerRspackPlugin({
      typescript: { configFile: path.resolve(__dirname, 'tsconfig.console.json') },
    }),
  ],
  devtool: isProd ? false : 'source-map',
  optimization: {
    chunkIds: isProd ? 'deterministic' : 'named',
    minimize: isProd,
  },
};
