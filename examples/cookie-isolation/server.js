const http = require('http');
const port = process.env.PORT || 8080;
const bay = process.env.DEVBAY_BAY || 'unknown';
http.createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/login') {
    // A session cookie, exactly as an application would set one.
    res.writeHead(200, {
      'Set-Cookie': `session=${bay}-session; Path=/`,
      'Content-Type': 'text/plain',
    });
    res.end(`logged in to ${bay}`);
    return;
  }
  // Report what the browser actually sent.
  res.writeHead(200, {'Content-Type': 'text/plain'});
  res.end(`bay=${bay} host=${req.headers.host} cookie=${req.headers.cookie || '(none)'}`);
}).listen(port, '0.0.0.0');
