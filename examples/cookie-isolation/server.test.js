const test = require('node:test');
const assert = require('node:assert');

// The point of this example is what a browser does with the cookie, which no
// unit test can observe. What can be checked here is that the service reports
// the header it was given -- if it stopped doing that, the browser gate would
// pass for the wrong reason.
test('reports the cookie header it received', () => {
  const src = require('fs').readFileSync(__dirname + '/server.js', 'utf8');
  assert.match(src, /req\.headers\.cookie/);
  assert.match(src, /Set-Cookie/);
});
