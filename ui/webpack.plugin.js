const path = require('path');
const MiniCssExtractPlugin = require('mini-css-extract-plugin');
const ForkTsCheckerWebpackPlugin = require('fork-ts-checker-webpack-plugin');
const { ConsoleRemotePlugin } = require('@openshift-console/dynamic-plugin-sdk-webpack');

const pluginMetadata = require('./src/plugin/plugin-manifest.json');
const { extensions } = require('./console-extensions.json');

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
      typescript: { configFile: path.resolve(__dirname, 'tsconfig.json') },
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
};
