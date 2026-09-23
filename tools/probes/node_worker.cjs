// Experimental ooth integration fixture, not a supported Node adapter.
// Windows requires Node 26.1+ with node:ffi and the private tcp_wrap binding.
const fs = require('node:fs');
const http = require('node:http');

function event(kind, fields = {}) {
  const values = { v: 1, event: kind, ts: BigInt(Date.now()) * 1000000n, ...fields };
  console.log(Object.entries(values).map(([key, value]) => `${key}=${value}`).join(' '));
}

let sequence = 0;
const server = http.createServer((request, response) => {
  const id = ++sequence;
  const started = process.hrtime.bigint();
  event('start', { id });
  response.once('finish', () => {
    event('end', { id, duration_ns: process.hrtime.bigint() - started });
  });
  response.writeHead(200, { Connection: 'close', 'Content-Type': 'text/plain' });
  response.flushHeaders();
  setTimeout(() => {
    response.end(`worker=${process.pid} request=${id}\n`);
  }, Number(request.url.slice(1)) || 0);
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
