// Async I/O pressure fixture. Same experimental stdin adoption as node_worker.cjs.
const http = require('node:http');
const http2 = require('node:http2');
const h2c = process.env.TEST_PRESSURE_H2C === '1';
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
let listener = { fd: 0 };
if (process.platform === 'win32') {
  const { dlopen } = require('node:ffi');
  const { lib, functions } = dlopen('kernel32.dll', {
    GetStdHandle: { arguments: ['uint32'], return: 'uint64' },
  });
  const descriptor = Number(functions.GetStdHandle(0xfffffff6));
  lib.close();
  const { TCP, constants } = process.binding('tcp_wrap');
  listener = new TCP(constants.SERVER);
  const error = listener.open(descriptor);
  if (error) throw new Error(`uv_tcp_open: ${error}`);
}
function stop() {
  server.close();
  for (const session of sessions) session.close();
}
process.on('SIGINT', stop);
process.on(process.platform === 'win32' ? 'SIGBREAK' : 'SIGTERM', stop);
server.listen(listener, () => event('ready'));
