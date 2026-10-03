const path = require('path');
const MiniCssExtractPlugin = require('mini-css-extract-plugin');
const ForkTsCheckerWebpackPlugin = require('fork-ts-checker-webpack-plugin');
const { ConsoleRemotePlugin } = require('@openshift-console/dynamic-plugin-sdk-webpack');

const compatibility = require('./console-compatibility.json');
const consoleVersion = process.env.CONSOLE_VERSION || '4.19';
const compatibilityProfile = compatibility[consoleVersion];
if (!compatibilityProfile) {
  throw new Error(`Unsupported console version ${consoleVersion}`);
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

const configuration = (isProd) => ({
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
        use: ['swc-loader'],
      },
      {
        test: /\.css$/,
        use: [MiniCssExtractPlugin.loader, 'css-loader'],
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
    new MiniCssExtractPlugin({ filename: isProd ? 'plugin-[contenthash].css' : 'plugin.css' }),
    new ConsoleRemotePlugin({ pluginMetadata, extensions }),
    new ForkTsCheckerWebpackPlugin({
      typescript: { configFile: path.resolve(__dirname, 'tsconfig.console.json') },
    }),
  ],
  devServer: {
    port: 9001,
    static: path.resolve(__dirname, 'dist/plugin'),
    allowedHosts: 'all',
    headers: { 'Access-Control-Allow-Origin': '*' },
    devMiddleware: { writeToDisk: true },
  },
  devtool: isProd ? false : 'source-map',
  optimization: {
    chunkIds: isProd ? 'deterministic' : 'named',
    minimize: isProd,
    splitChunks: { chunks: 'all' },
  },
});

module.exports = (_env, argv) => configuration(argv.mode === 'production');
