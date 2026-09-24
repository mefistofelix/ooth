const net = require('node:net');
const mode = process.argv[2] || 'fd';
const server = net.createServer(socket => socket.end(`${process.pid}\n`));
server.on('error', error => {
  console.error(error);
  process.exitCode = 1;
});

if (['native', 'stdhandle', 'osfhandle'].includes(mode)) {
  // Diagnostic only: private Node API. stdhandle/osfhandle use only stdin;
  // native is the older control experiment with an extra inherited handle.
  let descriptor;
  if (mode === 'native') {
    descriptor = Number(process.env.HANDOFF_HANDLE);
  } else {
    const { dlopen } = require('node:ffi');
    const kernel = mode === 'stdhandle';
    const symbol = kernel ? 'GetStdHandle' : '_get_osfhandle';
    const library = dlopen(kernel ? 'kernel32.dll' : 'ucrtbase.dll', {
      [symbol]: {
        arguments: [kernel ? 'uint32' : 'int32'],
        return: 'int64',
      },
    });
    descriptor = Number(library.functions[symbol](kernel ? 0xfffffff6 : 0));
    library.lib.close();
    console.error(`${symbol} stdin=${descriptor}`);
  }
  const { TCP, constants } = process.binding('tcp_wrap');
  const handle = new TCP(constants.SERVER);
  const error = handle.open(descriptor);
  if (error) throw new Error(`uv_tcp_open: ${error}`);
  server.listen(handle, () => console.log('ready'));
} else if (mode === 'stdin') {
  server.listen(process.stdin, () => console.log('ready'));
} else if (mode === 'fd') {
  server.listen({ fd: 0 }, () => console.log('ready'));
} else {
  throw new Error('usage: node node_handoff.cjs [fd|stdin|native|stdhandle|osfhandle]');
}
