// Experimental ooth integration fixture, not a supported Node adapter.
// Windows requires Node 26.1+ with node:ffi and the private tcp_wrap binding.
const fs = require('node:fs');
const http = require('node:http');
const http2 = require('node:http2');
const h2c = process.env.TEST_NODE_H2C === '1';

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
  });
  response.writeHead(200, h2c ? { 'Content-Type': 'text/plain' } : { Connection: 'close', 'Content-Type': 'text/plain' });
  response.flushHeaders();
  setTimeout(() => {
    response.end(`worker=${process.pid} request=${id} protocol=${h2c ? 'h2c' : 'http1'}\n`);
  }, Number(request.url.slice(1)) || 0);
};
const server = h2c ? http2.createServer({ settings: { maxConcurrentStreams: 1 } }, handleRequest) : http.createServer(handleRequest);
const sessions = new Set();
server.on('session', session => {
  sessions.add(session);
  session.once('close', () => sessions.delete(session));
});
server.on('error', error => {
  console.error(error);
  process.exitCode = 1;
});

let stopping = false;
function stop() {
  if (stopping) return;
  stopping = true;
  server.close(error => {
    if (error) throw error;
    if (process.env.TEST_NODE_STOPFILE) {
      fs.appendFileSync(process.env.TEST_NODE_STOPFILE, `${process.pid}\n`);
    }
  });
  for (const session of sessions) session.close();
}
process.on('SIGINT', stop);
process.on(process.platform === 'win32' ? 'SIGBREAK' : 'SIGTERM', stop);

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
server.listen(listener, () => event('ready'));
