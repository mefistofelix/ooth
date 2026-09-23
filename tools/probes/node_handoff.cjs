const net = require('node:net');
const mode = process.argv[2] || 'fd';
const server = net.createServer(socket => socket.end(`${process.pid}\n`));
server.on('error', error => {
  console.error(error);
  process.exitCode = 1;
});

if (mode === 'native') {
  // Diagnostic only: private Node API and an extra inherited native handle.
  // This is not the production ooth worker convention or a supported Node API.
  const { TCP, constants } = process.binding('tcp_wrap');
  const handle = new TCP(constants.SERVER);
  const error = handle.open(Number(process.env.HANDOFF_HANDLE));
  if (error) throw new Error(`uv_tcp_open: ${error}`);
  server.listen(handle, () => console.log('ready'));
} else if (mode === 'stdin') {
  server.listen(process.stdin, () => console.log('ready'));
} else if (mode === 'fd') {
  server.listen({ fd: 0 }, () => console.log('ready'));
} else {
  throw new Error('usage: node node_handoff.cjs [fd|stdin|native]');
}
