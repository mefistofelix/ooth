// Bounded RFC6455 echo fixture for the node:http upgrade event. This is not a
// general WebSocket library: tests use masked, unfragmented frames <=125 bytes.
// Socketify/TrueAsync use their own native WebSocket implementations instead.
const { createHash } = require('node:crypto');

module.exports = function installWebSocket(server, event, nextID) {
  const sessions = new Set();
  server.on('upgrade', (request, socket, head) => {
    if (request.url !== '/ws' || request.headers['sec-websocket-version'] !== '13') {
      socket.destroy();
      return;
    }
    const accept = createHash('sha1').update(request.headers['sec-websocket-key'] + '258EAFA5-E914-47DA-95CA-C5AB0DC85B11').digest('base64');
    socket.write(`HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${accept}\r\n\r\n`);
    const id = nextID();
    const started = process.hrtime.bigint();
    event('start', { id });
    sessions.add(socket);
    socket.once('close', () => {
      sessions.delete(socket);
      event('end', { id, duration_ns: process.hrtime.bigint() - started });
    });
    socket.on('error', error => {
      if (error.code !== 'ECONNRESET' && error.code !== 'EPIPE') console.error(error);
    });
    let buffer = Buffer.alloc(0);
    function frame(opcode, payload) {
      socket.write(Buffer.concat([Buffer.from([0x80 | opcode, payload.length]), payload]));
    }
    function receive(data) {
      buffer = Buffer.concat([buffer, data]);
      while (buffer.length >= 2) {
        const length = buffer[1] & 127;
        if (!(buffer[0] & 128) || !(buffer[1] & 128) || length > 125) {
          socket.destroy(new Error('Frame outside the bounded test protocol'));
          return;
        }
        if (buffer.length < 6 + length) return;
        const opcode = buffer[0] & 15;
        const payload = Buffer.from(buffer.subarray(6, 6 + length));
        for (let index = 0; index < length; index++) payload[index] ^= buffer[2 + index % 4];
        buffer = buffer.subarray(6 + length);
        if (opcode === 8) {
          frame(8, payload);
          socket.end();
          return;
        }
        if (opcode === 9) frame(10, payload);
        else if (opcode === 1 || opcode === 2) frame(opcode, payload);
      }
    }
    socket.on('data', receive);
    if (head.length) receive(head);
  });
  return function stopWebSockets() {
    for (const socket of sessions) {
      // Going Away. Keep reading until the client's close acknowledgement.
      socket.write(Buffer.from([0x88, 2, 3, 233]));
    }
  };
};
