// Async I/O pressure fixture. Same inherited-handle adapter as node_worker.cjs.
const http = require('node:http');
const http2 = require('node:http2');
const h2c = process.env.TEST_PRESSURE_H2C === '1';
const { listenOptions, watchStop } = require('../listener.cjs');
const now = () => BigInt(Date.now()) * 1000000n;
function event(kind, fields = {}) {
  console.log(Object.entries({ v: 1, event: kind, ts: now(), ...fields })
    .map(([key, value]) => `${key}=${value}`).join(' '));
}
function handle(request, response) {
  const query = new URL(request.url, 'http://localhost').searchParams;
  const id = query.get('id');
  const delay = Number(query.get('delay'));
  const accepted = h2c ? request.stream.session.pressureAccepted : request.socket.pressureAccepted;
  const started = now();
  const monotonic = process.hrtime.bigint();
  event('start', { id });
  const finish = () => {
    event('end', { id, duration_ns: process.hrtime.bigint() - monotonic });
    response.writeHead(200, h2c ? {} : { Connection: 'close' });
    response.end(`${id} ${process.pid} ${accepted} ${started}\n`);
  };
  if (process.env.TEST_PRESSURE_BLOCK === '1') {
    // Deliberately block the JS event loop, unlike an asynchronous I/O delay.
    while (process.hrtime.bigint() - monotonic < BigInt(delay) * 1000000n) {}
    finish();
  } else {
    setTimeout(finish, delay);
  }
}
const server = h2c ? http2.createServer(handle) : http.createServer(handle);
const sessions = new Set();
server.on('connection', connection => { connection.pressureAccepted = now(); });
server.on('session', session => {
  session.pressureAccepted = now();
  sessions.add(session);
  session.on('close', () => sessions.delete(session));
});
let control;
function stop() {
  control?.close();
  server.close();
  for (const session of sessions) session.close();
}
process.on('SIGINT', stop);
process.on(process.platform === 'win32' ? 'SIGBREAK' : 'SIGTERM', stop);
control = watchStop(stop);
server.listen(listenOptions(), () => event('ready'));
