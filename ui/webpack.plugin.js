const path = require('path');
const MiniCssExtractPlugin = require('mini-css-extract-plugin');
const { ConsoleRemotePlugin } = require('@openshift-console/dynamic-plugin-sdk-webpack');
const { extensions } = require('./console-extensions.json');
const pluginMetadata = require('./src/plugin/plugin-manifest.json');

module.exports = (env, argv) => {
  const isProd = argv.mode === 'production';
  return {
    entry: {},
    output: {
      path: path.resolve(__dirname, 'dist/plugin'),
      publicPath: 'auto',
      filename: isProd ? '[name]-bundle-[contenthash].min.js' : '[name]-bundle.js',
      chunkFilename: isProd ? '[name]-chunk-[contenthash].min.js' : '[name]-chunk.js',
      clean: true,
    },
    resolve: {
      extensions: ['.tsx', '.ts', '.js'],
      alias: { '@': path.resolve(__dirname, 'src') },
    },
    module: {
      rules: [
        {
          test: /\.tsx?$/,
          use: 'ts-loader',
          exclude: /node_modules/,
        },
        {
          test: /\.css$/,
          use: [MiniCssExtractPlugin.loader, 'css-loader'],
        },
        {
          test: /\.(woff2?|ttf|eot|svg)$/,
          type: 'asset/resource',
        },
      ],
    },
    plugins: [
      new MiniCssExtractPlugin({ filename: isProd ? 'plugin-[contenthash].css' : 'plugin.css' }),
      new ConsoleRemotePlugin({
        pluginMetadata,
        extensions,
      }),
    ],
    devServer: {
      port: 9001,
      static: path.resolve(__dirname, 'dist/plugin'),
      headers: { 'Access-Control-Allow-Origin': '*' },
    },
    optimization: {
      splitChunks: { chunks: 'all' },
    },
  };
};
