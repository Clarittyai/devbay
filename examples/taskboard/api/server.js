// A tiny API with no dependencies, so the example boots in seconds and needs
// no registry access. Redis is spoken directly over its wire protocol.
const http = require('http');
const net = require('net');

const REDIS = new URL(process.env.REDIS_URL || 'redis://cache:6379');

function redis(...args) {
  return new Promise((resolve, reject) => {
    const sock = net.createConnection(Number(REDIS.port), REDIS.hostname);
    const cmd = `*${args.length}\r\n` + args.map(a => `$${Buffer.byteLength(a)}\r\n${a}\r\n`).join('');
    let buf = '';
    sock.on('connect', () => sock.write(cmd));
    sock.on('data', d => {
      buf += d.toString();
      if (buf.endsWith('\r\n')) { sock.end(); resolve(buf); }
    });
    sock.on('error', reject);
    setTimeout(() => { sock.destroy(); reject(new Error('redis timeout')); }, 3000);
  });
}

// A task may carry a priority, stored as "priority:title".
function encodeTask(params) {
  const title = params.get('title') || 'untitled';
  const priority = params.get('priority');
  return priority ? priority + ':' + title : title;
}

const parseList = raw => raw.split('\r\n').filter((l, i) => i > 0 && l && !l.startsWith('$') && !l.startsWith('*'));

module.exports = { encodeTask, parseList };

// Only listen when run directly, so a test can require this file for its
// helpers without starting a second server on the same port.
if (require.main !== module) return;

http.createServer(async (req, res) => {
  const url = new URL(req.url, 'http://x');
  res.setHeader('Access-Control-Allow-Origin', '*');
  res.setHeader('Content-Type', 'application/json');
  try {
    if (url.pathname === '/healthz') {
      // Reports what it was wired to, which is what makes the three address
      // planes visible: DATABASE_URL is a container address, PUBLIC_URL is a
      // browser one, and they are not the same string.
      await redis('PING');
      res.end(JSON.stringify({
        ok: true,
        bay: process.env.DEVBAY_BAY || 'unknown',
        database: (process.env.DATABASE_URL || '').replace(/:[^:@]*@/, ':***@'),
      }));
      return;
    }
    if (url.pathname === '/leak') {
      // Applications print their own configuration all the time -- a startup
      // banner, a debug endpoint, an error from a client library quoting the
      // credential it just used. devbay scrubs what it returns, and this is
      // here so the acceptance suite can prove that against a real leak.
      console.log('config dump:', JSON.stringify(process.env));
      res.end(JSON.stringify({ dumped: true }));
      return;
    }
    if (url.pathname === '/tasks' && req.method === 'POST') {
      await redis('RPUSH', 'tasks', encodeTask(url.searchParams));
      res.statusCode = 201;
      res.end(JSON.stringify({ created: true }));
      return;
    }
    if (url.pathname === '/tasks') {
      res.end(JSON.stringify({ tasks: parseList(await redis('LRANGE', 'tasks', '0', '-1')) }));
      return;
    }
    res.statusCode = 404;
    res.end(JSON.stringify({ error: 'not found' }));
  } catch (err) {
    res.statusCode = 500;
    res.end(JSON.stringify({ error: String(err) }));
  }
}).listen(4000, '0.0.0.0', () => console.log('api listening on 4000'));
