<?php
// Diagnostic for the official Windows TrueAsync 0.10.0 binary, not a public API.
// A single PHP thread temporarily intercepts the native server's listener adoption.
use TrueAsync\HttpServer;
use TrueAsync\HttpServerConfig;

$socket = (int) getenv('OOTH_LISTEN_HANDLE');
if ($socket <= 0 || fgets(STDIN) !== "ordinary stdin\n") {
    throw new RuntimeException('Missing inherited handle or ordinary stdin');
}
$api = FFI::cdef('
    typedef struct zend_async_listen_event_s zend_async_listen_event_t;
    typedef zend_async_listen_event_t *(*listen_fd_fn)(uintptr_t, int, uint32_t, size_t);
    extern listen_fd_fn zend_async_socket_listen_fd_fn;
', dirname(PHP_BINARY) . '/php8ts.dll');
$winsock = FFI::cdef('int closesocket(uintptr_t socket);', 'ws2_32.dll');

// Copy the value: keeping a reference to the exported variable would recurse.
$original = $api->new('listen_fd_fn[1]');
$original[0] = $api->zend_async_socket_listen_fd_fn;
$adopt = function ($temporary, $backlog, $flags, $extra) use ($api, $original, $socket, $winsock) {
    $api->zend_async_socket_listen_fd_fn = $original[0];
    // The server has already bound a temporary loopback listener on port 0.
    // Replace its adoption duplicate; its original is still owned by the server.
    $winsock->closesocket($temporary);
    fwrite(STDERR, "native server adopting inherited handle=$socket\n");
    return ($original[0])($socket, $backlog, $flags, $extra);
};
$server = new HttpServer((new HttpServerConfig())->addListener('127.0.0.1', 0)->setWorkers(1));
$server->addHttpHandler(function ($request, $response) {
    Async\delay(20);
    $response->setStatusCode(200)->setHeader('Content-Type', 'text/plain')->setBody(getmypid() . "\n");
});
$api->zend_async_socket_listen_fd_fn = $adopt;
echo "ready\n";
try {
    $server->start();
} finally {
    $api->zend_async_socket_listen_fd_fn = $original[0];
}
