// Experimental ooth integration fixture for Node, Bun and Deno.
// Windows uses the private tcp_wrap binding; only legacy stdin needs node:ffi.
const fs = require('node:fs');
const http = require('node:http');
const http2 = require('node:http2');
const h2c = process.env.TEST_NODE_H2C === '1';
const { listenOptions, watchStop } = require('./listener.cjs');

function event(kind, fields = {}) {
  const values = { v: 1, event: kind, ts: BigInt(Date.now()) * 1000000n, ...fields };
  console.log(Object.entries(values).map(([key, value]) => `${key}=${value}`).join(' '));
}

let sequence = 0;
const handleRequest = (request, response) => {
  const id = ++sequence;
  const started = process.hrtime.bigint();
  const session = h2c ? request.stream.session : null;
  event('start', { id });
  response.once('finish', () => {
    event('end', { id, duration_ns: process.hrtime.bigint() - started });
    // Rotate test h2c sessions so later requests exercise newly spawned
    // workers; existing multiplexed connections stay with their acceptor.
    if (session) session.close();
    else if (process.versions.deno) request.socket.destroySoon();
  });
  response.writeHead(200, h2c ? { 'Content-Type': 'text/plain' } : { Connection: 'close', 'Content-Type': 'text/plain' });
  response.flushHeaders();
  setTimeout(() => {
    response.end(`worker=${process.pid} request=${id} protocol=${h2c ? 'h2c' : 'http1'}\n`);
  }, Number(request.url.slice(1)) || 0);
};
const server = h2c ? http2.createServer({ settings: { maxConcurrentStreams: 1 } }, handleRequest) : http.createServer(handleRequest);
const stopWebSockets = !h2c ? require('./javascript/websocket.cjs')(server, event, () => ++sequence) : () => {};
const windows = process.platform === 'win32';
const owner = windows && process.versions.bun ? require('./javascript/bun-windows.cjs')(server)
  : windows && process.versions.deno ? require('./javascript/deno-windows.cjs')(server)
  : process.versions.bun ? require('node:net').createServer(socket => server.emit('connection', socket))
  : server;
const sessions = new Set();
server.on('session', session => {
  sessions.add(session);
  session.once('close', () => sessions.delete(session));
});
server.on('error', error => {
  console.error(error);
  process.exitCode = 1;
});
if (owner !== server) owner.on('error', error => { throw error; });

let stopping = false;
let control;
function stop() {
  if (stopping) return;
  stopping = true;
  control?.close();
  stopWebSockets();
  owner.close(error => {
    if (error) throw error;
    if (process.env.TEST_NODE_STOPFILE) {
      fs.appendFileSync(process.env.TEST_NODE_STOPFILE, `${process.pid}\n`);
    }
    // The FFI adapters retain a native wait thread until process exit.
    if (owner !== server) setImmediate(() => process.stdout.write('', () => process.exit(0)));
  });
  for (const session of sessions) session.close();
}
process.on('SIGINT', stop);
process.on(process.platform === 'win32' ? 'SIGBREAK' : 'SIGTERM', stop);

control = watchStop(stop);
owner.listen(owner === server ? listenOptions() : { fd: Number(process.env.OOTH_LISTEN_HANDLE) }, () => event('ready'));
