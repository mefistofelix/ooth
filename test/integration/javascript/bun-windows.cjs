// Windows feasibility shim. A dedicated worker thread waits in Winsock; the
// application's thread adopts connected sockets into Bun's HTTP server.
const { isMainThread, parentPort, workerData, Worker } = require('node:worker_threads');
const { dlopen, ptr } = require('bun:ffi');
const trace = message => { if (process.env.TEST_JS_TRACE) require('node:fs').writeSync(2, `${process.pid} ${message}\n`); };
const { symbols } = dlopen('ws2_32.dll', {
  WSAStartup: { args: ['u16', 'ptr'], returns: 'i32' },
  ioctlsocket: { args: ['u64', 'i32', 'ptr'], returns: 'i32' },
  WSAPoll: { args: ['ptr', 'u32', 'i32'], returns: 'i32' },
  accept: { args: ['u64', 'ptr', 'ptr'], returns: 'u64' },
  closesocket: { args: ['u64'], returns: 'i32' },
  WSAGetLastError: { args: [], returns: 'i32' },
});

if (!isMainThread) {
  const state = new Int32Array(workerData.state);
  const listener = BigInt(workerData.listener);
  const startup = new Uint8Array(512);
  if (symbols.WSAStartup(0x0202, ptr(startup))) throw new Error('WSAStartup failed');
  const nonblocking = new Uint32Array([1]);
  if (symbols.ioctlsocket(listener, -2147195266, ptr(nonblocking))) throw new Error('FIONBIO failed');
  const poll = new Uint8Array(16);
  const view = new DataView(poll.buffer);
  view.setBigUint64(0, listener, true);
  view.setInt16(8, 0x0100, true);
  parentPort.postMessage('ready');
  while (!Atomics.load(state, 0)) {
    const count = symbols.WSAPoll(ptr(poll), 1, -1);
    if (Atomics.load(state, 0)) break;
    if (count < 0) throw new Error(`WSAPoll: ${symbols.WSAGetLastError()}`);
    const socket = symbols.accept(listener, null, null);
    if (BigInt(socket) === 0xffffffffffffffffn) {
      const error = symbols.WSAGetLastError();
      if (error === 10035) continue;
      throw new Error(`accept: ${error}`);
    }
    parentPort.postMessage(Number(socket));
    trace(`accepted ${socket}`);
  }
} else {
  module.exports = function acceptInto(server) {
    const net = require('node:net');
    const owner = new (require('node:events').EventEmitter)();
    const state = new Int32Array(new SharedArrayBuffer(4));
    const listener = BigInt(process.env.OOTH_LISTEN_HANDLE);
    const connections = new Set();
    let closed;
    function finish() {
      if (closed && connections.size === 0) {
        const callback = closed;
        closed = null;
        callback();
      }
    }
    owner.listen = (_options, ready) => {
      const worker = new Worker(__filename, { workerData: { state: state.buffer, listener: String(listener) } });
      worker.on('error', error => owner.emit('error', error));
      worker.on('message', fd => {
        trace(`message ${fd}`);
        if (fd === 'ready') return ready();
        if (Atomics.load(state, 0)) { symbols.closesocket(BigInt(fd)); return; }
        const connection = new net.Socket();
        connections.add(connection);
        // A peer reset ends this connection, not the listening worker.
        connection.on('error', error => { if (error.code !== 'ECONNRESET' && error.code !== 'EPIPE') owner.emit('error', error); });
        connection.once('close', () => { connections.delete(connection); finish(); });
        server.emit('connection', connection);
        connection.connect({ fd, fdIsRawSocket: true });
      });
    };
    owner.close = callback => {
      Atomics.store(state, 0, 1);
      symbols.closesocket(listener);
      closed = callback;
      finish();
    };
    // As in the Deno probe, the fixture exits after drain; the wait thread/DLL
    // are process-lifetime resources, not a certified unloadable library.
    return owner;
  };
}
