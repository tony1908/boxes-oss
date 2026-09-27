import assert from 'node:assert/strict';
import { test } from 'node:test';
import { loginRedirect, postLoginDestination } from '../app/lib/auth-redirect.mjs';

test('a protected API 401 redirects to login with the intended path and query', () => {
  assert.equal(
    loginRedirect(401, '/sandboxes', { pathname: '/nodes/', search: '?status=healthy' }),
    '/login/?next=%2Fnodes%2F%3Fstatus%3Dhealthy',
  );
});

test('authentication 401s stay on the auth form so invalid credentials can be shown', () => {
  assert.equal(loginRedirect(401, '/auth/login', { pathname: '/login/', search: '' }), null);
});

test('non-authentication failures do not redirect', () => {
  assert.equal(loginRedirect(500, '/sandboxes', { pathname: '/', search: '' }), null);
});

test('post-login navigation accepts only safe same-origin paths', () => {
  assert.equal(postLoginDestination('?next=%2Fsandboxes%2F%3Fstate%3Dready', 'https://boxes.example'), '/sandboxes/?state=ready');
  assert.equal(postLoginDestination('?next=https%3A%2F%2Fevil.example', 'https://boxes.example'), '/');
  assert.equal(postLoginDestination('?next=%2F%2Fevil.example', 'https://boxes.example'), '/');
  assert.equal(postLoginDestination('?next=%2Flogin%2F', 'https://boxes.example'), '/');
});
