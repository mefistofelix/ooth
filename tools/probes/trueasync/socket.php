<?php
// Low-level adoption probe, not the built-in HTTP server adapter.
fwrite(STDERR, "script started\n");
// fd 0 hits a TrueAsync 0.10.0 poller bug on Linux. This wrapper duplicates
// the input descriptor; the listener remains the same kernel socket.
$descriptor = getenv('OOTH_LISTEN_HANDLE');
if ($descriptor !== false && fgets(STDIN) !== "ordinary stdin\n") {
    throw new RuntimeException('Ordinary stdin is not usable');
}
$input = fopen('php://fd/' . ($descriptor === false ? '0' : $descriptor), 'r+');
$listener = socket_import_stream($input);
if ($listener === false) {
    throw new RuntimeException('Cannot adopt inherited listener');
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
