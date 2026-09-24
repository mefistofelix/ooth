// Test adapter: the environment carries an inherited fd (Linux) or SOCKET
// (Windows). The stdin path remains for explicit legacy activation fixtures.
function listenOptions() {
  let descriptor = Number(process.env.OOTH_LISTEN_HANDLE ?? 0);
  if (process.platform !== 'win32') return { fd: descriptor };
  if (process.env.OOTH_LISTEN_HANDLE === undefined) {
    const { dlopen } = require('node:ffi');
    const { lib, functions } = dlopen('kernel32.dll', {
      GetStdHandle: { arguments: ['uint32'], return: 'uint64' },
    });
    descriptor = Number(functions.GetStdHandle(0xfffffff6));
    lib.close();
  }
  const { TCP, constants } = process.binding('tcp_wrap');
  const listener = new TCP(constants.SERVER);
  const error = listener.open(descriptor);
  if (error) throw new Error(`uv_tcp_open: ${error}`);
  return listener;
}

function watchStop(stop) {
  if (process.env.OOTH_LISTEN_HANDLE === undefined) return null;
  const input = require('node:readline').createInterface({ input: process.stdin });
  // readline.close() alone leaves the pipe handle alive on Windows.
  input.once('close', () => process.stdin.destroy());
  input.on('line', line => {
    const fields = Object.fromEntries(line.trim().split(/\s+/).map(field => field.split('=')));
    if (fields.v === '1' && fields.event === 'stop') {
      input.close();
      stop();
    }
  });
  return input;
}

module.exports = { listenOptions, watchStop };
