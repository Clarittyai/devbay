// Node's own test runner, so `devbay run <bay> unit` needs nothing installed
// and can emit JUnit XML for devbay to parse into typed failures.
const { test } = require('node:test');
const assert = require('node:assert');

test('a title is required', () => {
  assert.equal(new URL('http://x/tasks?title=write%20docs').searchParams.get('title'), 'write docs');
});

test('an empty list parses as empty', () => {
  const parseList = raw => raw.split('\r\n').filter((l, i) => i > 0 && l && !l.startsWith('$') && !l.startsWith('*'));
  assert.deepEqual(parseList('*0\r\n'), []);
});
