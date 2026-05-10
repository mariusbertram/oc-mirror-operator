/**
 * webpack.dev.js — standalone dev harness for the Console Plugin UI.
 *
 * Unlike webpack.plugin.js this config does NOT use ConsoleRemotePlugin, so the
 * output is a plain React SPA that runs without an OpenShift Console.
 *
 * Usage:  npm --prefix ui run dev
 *
 * The dev server runs on port 9002 and proxies all /api/ requests to the local
 * Go plugin backend at https://localhost:9443.  See docs/developer-guide.md §8.
 */
const path = require('path');
const ForkTsCheckerWebpackPlugin = require('fork-ts-checker-webpack-plugin');
const { registerMockHandlers } = require('./src/dev/mocks');

const API_URL = process.env.API_URL || 'https://localhost:9443';
const MOCK = process.env.MOCK === 'true';

module.exports = {
  mode: 'development',
  entry: './dev/index.tsx',
  context: path.resolve(__dirname, 'src'),
  output: {
    path: path.resolve(__dirname, 'dist/dev'),
    filename: 'bundle.js',
    publicPath: '/',
    clean: true,
  },
  resolve: {
    extensions: ['.tsx', '.ts', '.js', '.jsx'],
    alias: { '@': path.resolve(__dirname, 'src') },
  },
  module: {
    rules: [
      {
        test: /\.(jsx?|tsx?)$/,
        exclude: /\/node_modules\//,
        use: ['swc-loader'],
      },
      {
        // Use style-loader so CSS is injected at runtime — no separate CSS file needed.
        test: /\.css$/,
        use: ['style-loader', 'css-loader'],
      },
      {
        test: /\.(png|jpg|jpeg|gif|svg|woff2?|ttf|eot|otf)(\?.*$|$)/,
        type: 'asset/resource',
        generator: { filename: 'assets/[name][ext]' },
      },
      {
        test: /\.(m?js)$/,
        resolve: { fullySpecified: false },
      },
    ],
  },
  plugins: [
    new ForkTsCheckerWebpackPlugin({
      typescript: { configFile: path.resolve(__dirname, 'tsconfig.json') },
    }),
  ],
  devServer: {
    port: 9002,
    // Serve the static index.html from ui/public/.
    static: path.resolve(__dirname, 'public'),
    historyApiFallback: true,
    // In mock mode the setupMiddlewares hook intercepts all /api/ requests
    // before they reach the proxy, so no backend is needed.
    proxy: MOCK
      ? []
      : [{ context: ['/api'], target: API_URL, secure: false, changeOrigin: true }],
    setupMiddlewares: (middlewares, devServer) => {
      if (MOCK) registerMockHandlers(devServer.app);
      return middlewares;
    },
  },
  devtool: 'source-map',
  optimization: {
    chunkIds: 'named',
    minimize: false,
  },
};
