import tseslint from 'typescript-eslint';

export default tseslint.config(
  { ignores: ['dist/**', 'out-test/**', 'webview/dist/**'] },
  ...tseslint.configs.recommended,
  {
    files: ['src/**/*.ts', 'webview/**/*.ts'],
    rules: {
      '@typescript-eslint/no-unused-vars': ['error', { argsIgnorePattern: '^_' }],
    },
  },
);
