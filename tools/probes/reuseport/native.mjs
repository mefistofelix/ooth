// Public native HTTP/1 server APIs, without FFI or an inherited listener.
import process from 'node:process';
import readline from 'node:readline';

const address = new URL(process.argv[2]);
const options = { hostname: address.hostname, port: Number(address.port), reusePort: true };
let sequence = 0;
const websockets = new Set();
function event(kind, fields = {}) {
  const values = { v: 1, event: kind, ts: BigInt(Date.now()) * 1000000n, ...fields };
  console.log(Object.entries(values).map(([key, value]) => `${key}=${value}`).join(' '));
}
async function handler(request) {
  if (new URL(request.url).pathname === '/ws') {
    if (process.versions.bun) {
      if (server.upgrade(request, { data: {} })) return;
      return new Response('Upgrade failed', { status: 400 });
    }
    const { socket, response } = Deno.upgradeWebSocket(request);
    socket.addEventListener('open', () => openSocket(socket));
    socket.addEventListener('message', message => socket.send(message.data));
    socket.addEventListener('close', () => closeSocket(socket));
    return response;
  }
  const id = ++sequence;
  const started = process.hrtime.bigint();
  event('start', { id });
  await new Promise(resolve => setTimeout(resolve, Number(new URL(request.url).pathname.slice(1)) || 0));
  const protocol = process.env.TEST_NODE_H2C === '1' ? 'h2c' : 'http1';
  const response = new Response(`worker=${process.pid} request=${id} protocol=${protocol}\n`);
  event('end', { id, duration_ns: process.hrtime.bigint() - started });
  return response;
}
function openSocket(socket) {
  socket.oothRequest = { id: ++sequence, started: process.hrtime.bigint() };
  websockets.add(socket);
  event('start', { id: socket.oothRequest.id });
}
function closeSocket(socket) {
  websockets.delete(socket);
  event('end', { id: socket.oothRequest.id, duration_ns: process.hrtime.bigint() - socket.oothRequest.started });
}
const input = readline.createInterface({ input: process.stdin });
input.once('close', () => process.stdin.destroy());
let server;
if (process.versions.bun) {
  server = Bun.serve({ ...options, fetch: handler, websocket: {
    open: openSocket,
    message: (socket, message) => socket.send(message),
    close: closeSocket,
  } });
  event('ready');
} else {
  server = Deno.serve({ ...options, onListen: () => event('ready') }, handler);
}
input.on('line', async line => {
  if (!line.startsWith('v=1 event=stop ts=')) return;
  input.close();
  for (const socket of websockets) socket.close(1001, 'ooth stopping');
  if (process.versions.bun) await server.stop();
  else await server.shutdown();
});
