import assert from 'node:assert/strict';
import { test } from 'node:test';
import { authPageState } from '../app/lib/auth-status.mjs';

test('owner signup link appears only while initial setup is required', () => {
  assert.equal(authPageState('login', true).showCreateOwnerLink, true);
  assert.equal(authPageState('login', false).showCreateOwnerLink, false);
  assert.equal(authPageState('login', null).showCreateOwnerLink, false);
});

test('signup redirects to login once initial setup is complete', () => {
  assert.equal(authPageState('signup', false).redirectSignupToLogin, true);
  assert.equal(authPageState('signup', true).redirectSignupToLogin, false);
  assert.equal(authPageState('signup', null).redirectSignupToLogin, false);
});
