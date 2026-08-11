// Node's own test runner, so `devbay run <bay> unit` needs nothing installed
// and can emit JUnit XML for devbay to parse into typed failures.
const { test } = require('node:test');
const assert = require('node:assert');
const { encodeTask } = require('./server.js');

test('a title is required', () => {
  assert.equal(new URL('http://x/tasks?title=write%20docs').searchParams.get('title'), 'write docs');
});

test('an empty list parses as empty', () => {
  const parseList = raw => raw.split('\r\n').filter((l, i) => i > 0 && l && !l.startsWith('$') && !l.startsWith('*'));
  assert.deepEqual(parseList('*0\r\n'), []);
});

test('a task can carry a priority', () => {
  const params = new URL('http://x/tasks?title=ship&priority=high').searchParams;
  assert.equal(encodeTask(params), 'high:ship');
});

test('a task without a priority is stored as its title', () => {
  const params = new URL('http://x/tasks?title=ship').searchParams;
  assert.equal(encodeTask(params), 'ship');
});
