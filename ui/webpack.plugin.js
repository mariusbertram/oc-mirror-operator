const path = require('path');
const MiniCssExtractPlugin = require('mini-css-extract-plugin');
const { DynamicRemotePlugin } = require('@openshift-console/dynamic-plugin-sdk/webpack');

const pluginMetadata = require('./src/plugin/plugin-manifest.json');

module.exports = (env, argv) => ({
  entry: {},
  output: {
    path: path.resolve(__dirname, 'dist/plugin'),
    publicPath: 'auto',
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
    new MiniCssExtractPlugin({ filename: 'plugin.css' }),
    new DynamicRemotePlugin({
      pluginMetadata,
      extensions: require('./src/plugin/extensions').default,
    }),
  ],
  devServer: {
    port: 9001,
    static: path.resolve(__dirname, 'dist/plugin'),
    headers: { 'Access-Control-Allow-Origin': '*' },
  },
});
