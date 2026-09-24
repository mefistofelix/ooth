// Experimental Windows adapter: Winsock accepts in this worker; Deno owns
// each connected socket and runs its normal node:http implementation.
const net = require('node:net');
const { EventEmitter } = require('node:events');

module.exports = function acceptInto(server) {
  const { symbols } = Deno.dlopen('ws2_32.dll', {
    WSAStartup: { parameters: ['u16', 'buffer'], result: 'i32' },
    ioctlsocket: { parameters: ['usize', 'i32', 'buffer'], result: 'i32' },
    WSAPoll: { parameters: ['buffer', 'u32', 'i32'], result: 'i32', nonblocking: true },
    accept: { parameters: ['usize', 'pointer', 'pointer'], result: 'usize' },
    closesocket: { parameters: ['usize'], result: 'i32' },
    WSAGetLastError: { parameters: [], result: 'i32' },
  });
  const startup = symbols.WSAStartup(0x0202, new Uint8Array(512));
  if (startup) throw new Error(`WSAStartup: ${startup}`);
  const { TCP, constants } = process.binding('tcp_wrap');
  const listener = Number(process.env.OOTH_LISTEN_HANDLE);
  // Readiness can be consumed by another worker before this worker accepts.
  // accept must then return WSAEWOULDBLOCK rather than block the JS thread.
  if (symbols.ioctlsocket(listener, -2147195266, new Uint32Array([1])) !== 0) {
    throw new Error(`FIONBIO: ${symbols.WSAGetLastError()}`);
  }
  const poll = new Uint8Array(16); // WSAPOLLFD on Windows amd64.
  const view = new DataView(poll.buffer);
  view.setBigUint64(0, BigInt(listener), true);
  view.setInt16(8, 0x0100, true); // POLLRDNORM, including accept readiness.
  const owner = new EventEmitter();
  const connections = new Set();
  let stopping = false;
  let closed;
  function finish() {
    if (closed && connections.size === 0) {
      const callback = closed;
      closed = null;
      callback();
    }
  }
  async function acceptLoop() {
    while (!stopping) {
      // A blocking OS readiness wait on Deno's FFI pool, not timer polling.
      const count = await symbols.WSAPoll(poll, 1, -1);
      if (stopping) return;
      if (count < 0) throw new Error('WSAPoll failed');
      const socket = symbols.accept(listener, null, null);
      if (BigInt(socket) === 0xffffffffffffffffn) {
        const error = symbols.WSAGetLastError();
        if (error === 10035) continue; // Another worker won this accept.
        throw new Error(`accept: ${error}`);
      }
      if (BigInt(socket) > 0x7fffffffn) {
        symbols.closesocket(socket);
        throw new Error('TCPWrap.open cannot represent this handle');
      }
      const handle = new TCP(constants.SOCKET);
      const error = handle.open(Number(socket));
      if (error) throw new Error(`connected TCPWrap.open: ${error}`);
      const connection = new net.Socket({ handle, readable: true, writable: true });
      connections.add(connection);
      connection.once('close', () => { connections.delete(connection); finish(); });
      server.emit('connection', connection);
    }
  }
  owner.listen = (_options, ready) => {
    acceptLoop().catch(error => owner.emit('error', error));
    queueMicrotask(ready);
  };
  owner.close = callback => {
    stopping = true;
    symbols.closesocket(listener);
    closed = callback;
    finish();
  };
  // Keep the DLL loaded until process exit: an infinite WSAPoll may still be
  // returning on an FFI thread after closesocket. The fixture exits after drain.
  return owner;
};
