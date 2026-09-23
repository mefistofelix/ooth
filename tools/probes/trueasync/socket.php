<?php
// Low-level adoption probe, not the built-in HTTP server adapter.
fwrite(STDERR, "script started\n");
// fd 0 hits a TrueAsync 0.10.0 poller bug on Linux. This wrapper duplicates
// stdin before sockets adopts it; the listener remains the same kernel socket.
$input = fopen('php://fd/0', 'r+');
$listener = socket_import_stream($input);
if ($listener === false) {
    throw new RuntimeException('Cannot adopt stdin');
}
echo "ready\n";
Async\await(Async\spawn(function () use ($listener) {
    while ($client = socket_accept($listener)) {
        Async\spawn(function () use ($client) {
            Async\delay(50);
            socket_write($client, getmypid() . "\n");
            socket_close($client);
        });
    }
}));
