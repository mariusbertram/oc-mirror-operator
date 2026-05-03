const path = require('path');
const HtmlWebpackPlugin = require('html-webpack-plugin');
const MiniCssExtractPlugin = require('mini-css-extract-plugin');

module.exports = (env, argv) => {
  const isDev = argv.mode === 'development';

  return {
    entry: './src/dashboard/index.tsx',
    output: {
      path: path.resolve(__dirname, 'dist/dashboard'),
      filename: 'bundle.[contenthash].js',
      publicPath: '/',
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
          use: isDev ? ['style-loader', 'css-loader'] : [MiniCssExtractPlugin.loader, 'css-loader'],
        },
        {
          test: /\.(woff2?|ttf|eot|svg)$/,
          type: 'asset/resource',
        },
      ],
    },
    plugins: [
      new HtmlWebpackPlugin({
        template: './src/dashboard/index.html',
        filename: 'index.html',
      }),
      ...(isDev ? [] : [new MiniCssExtractPlugin({ filename: 'styles.[contenthash].css' })]),
    ],
    devServer: {
      port: 3000,
      historyApiFallback: true,
      proxy: [
        {
          context: ['/api'],
          target: 'http://localhost:8081',
          changeOrigin: true,
        },
      ],
    },
  };
};
