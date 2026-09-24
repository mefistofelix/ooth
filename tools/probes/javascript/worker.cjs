// Feasibility fixture: use runtime HTTP implementations, no custom HTTP parser.
const http = require('node:http');
const net = require('node:net');
const readline = require('node:readline');
const { listenOptions } = require('../listener.cjs');
const trace = message => { if (process.env.TEST_JS_TRACE) require('node:fs').writeSync(2, `${process.pid} ${message}\n`); };
const bunWindows = process.versions.bun && process.platform === 'win32';
if (bunWindows && process.env.TEST_BUN_DIRECT === '1') {
  // The adopted listener must stay nonblocking when another worker wins an
  // accept. Otherwise Bun can block its JS thread before reading stdin stop.
  const { dlopen, ptr } = require('bun:ffi');
  const library = dlopen('ws2_32.dll', {
    ioctlsocket: { args: ['u64', 'i32', 'ptr'], returns: 'i32' },
  });
  const nonblocking = new Uint32Array([1]);
  const error = library.symbols.ioctlsocket(BigInt(process.env.OOTH_LISTEN_HANDLE), -2147195266, ptr(nonblocking));
  library.close();
  if (error) throw new Error('ioctlsocket FIONBIO failed');
}

const server = http.createServer((request, response) => {
  response.writeHead(200, { Connection: 'close', 'Content-Type': 'text/plain' });
  response.flushHeaders();
  setTimeout(() => response.end(`${process.pid}\n`), Number(request.url.slice(1)) || 0);
});
// Bun's HTTP.listen ignores fd. Its TCP server accepts fd and the built-in
// node:http compatibility server explicitly supports injected connections.
const denoWindows = process.versions.deno && process.platform === 'win32' && process.env.TEST_DENO_DIRECT !== '1';
const owner = denoWindows ? require('./deno-windows.cjs')(server)
  : bunWindows && process.env.TEST_BUN_DIRECT !== '1' ? require('./bun-windows.cjs')(server)
  : process.versions.bun ? net.createServer(socket => server.emit('connection', socket))
  : server;
owner.on('error', error => { console.error(error); process.exit(1); });
const input = readline.createInterface({ input: process.stdin });
let checkedStdin = false;
let listening = false;
let stopping = false;
function ready() {
  if (checkedStdin && listening) console.log('ready');
}
input.on('line', line => {
  if (line === 'ordinary stdin' && !checkedStdin) {
    checkedStdin = true;
    ready();
  } else if (line.startsWith('v=1 event=stop ts=') && !stopping) {
    trace(`received stop connections=${owner._connections}`);
    stopping = true;
    input.close();
    owner.close(error => {
      trace('listener closed');
      if (error) throw error;
      // No remaining connections: all response bodies have been flushed.
      console.log('stopped');
      process.exit(0);
    });
  }
});
const options = process.versions.bun || denoWindows
  ? { fd: Number(process.env.OOTH_LISTEN_HANDLE) }
  : listenOptions();
owner.listen(options, () => { listening = true; ready(); });
