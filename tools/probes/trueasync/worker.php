<?php
// Experimental native HTTP worker. Windows uses the pinned release's FFI hook.
use TrueAsync\HttpServer;
use TrueAsync\HttpServerConfig;

function event(string $type, array $fields = []): void {
    $fields = ['v' => 1, 'event' => $type, 'ts' => (int) (microtime(true) * 1e9)] + $fields;
    $line = [];
    foreach ($fields as $key => $value) {
        $line[] = "$key=$value";
    }
    fwrite(STDOUT, implode(' ', $line) . "\n");
    fflush(STDOUT);
}

$bind = getenv('TEST_BIND_URL');
$api = null;
$config = (new HttpServerConfig())->setWorkers(1)->setShutdownTimeout(2);
if ($bind !== false) {
    $address = parse_url($bind);
    $config->addListener($address['host'], $address['port']);
} else {
    $socket = (int) getenv('OOTH_LISTEN_HANDLE');
    if ($socket <= 0) {
        throw new RuntimeException('Missing inherited listener');
    }
    $api = FFI::cdef('
    typedef struct zend_async_listen_event_s zend_async_listen_event_t;
    typedef zend_async_listen_event_t *(*listen_fd_fn)(uintptr_t, int, uint32_t, size_t);
    extern listen_fd_fn zend_async_socket_listen_fd_fn;
', PHP_OS_FAMILY === 'Windows' ? dirname(PHP_BINARY) . '/php8ts.dll' : null);
    $close = PHP_OS_FAMILY === 'Windows'
    ? FFI::cdef('int closesocket(uintptr_t socket);', 'ws2_32.dll')
    : FFI::cdef('int close(int fd);');
    $original = $api->new('listen_fd_fn[1]');
    $original[0] = $api->zend_async_socket_listen_fd_fn;
    $adopt = function ($temporary, $backlog, $flags, $extra) use ($api, $original, $socket, $close) {
        $api->zend_async_socket_listen_fd_fn = $original[0];
        if (PHP_OS_FAMILY === 'Windows') $close->closesocket($temporary);
        else $close->close($temporary);
        return ($original[0])($socket, $backlog, $flags, $extra);
    };
    $config->addListener('127.0.0.1', 0);
}
$server = new HttpServer($config);
$sequence = 0;
$websockets = [];
$server->addWebSocketHandler(function ($ws, $request) use (&$sequence, &$websockets) {
    $id = ++$sequence;
    $start = hrtime(true);
    $websockets[$id] = $ws;
    event('start', ['id' => $id]);
    try {
        while (($message = $ws->recv()) !== null) {
            if ($message->binary) $ws->sendBinary($message->data);
            else $ws->send($message->data);
        }
    } finally {
        unset($websockets[$id]);
        event('end', ['id' => $id, 'duration_ns' => hrtime(true) - $start]);
    }
});
$server->addHttpHandler(function ($request, $response) use (&$sequence) {
    $id = ++$sequence;
    $start = hrtime(true);
    event('start', ['id' => $id]);
    try {
        $delay = min(2000, max(0, (int) trim($request->getUri(), '/')));
        Async\delay($delay);
        $protocol = str_starts_with($request->getHttpVersion(), '2') ? 'h2c' : 'http1';
        $response->setStatusCode(200)->setHeader('Content-Type', 'text/plain');
        $response->end('worker=' . getmypid() . " request=$id protocol=$protocol\n");
    } finally {
        event('end', ['id' => $id, 'duration_ns' => hrtime(true) - $start]);
    }
});
$control = Async\spawn(function () use ($server, &$websockets) {
    while (!$server->isRunning()) Async\delay(1);
    event('ready');
    while (($line = fgets(STDIN)) !== false) {
        if (str_starts_with($line, 'v=1 event=stop ts=')) {
            foreach ($websockets as $ws) $ws->close(TrueAsync\WebSocketCloseCode::GOING_AWAY, 'ooth stopping');
            $server->stop();
            return;
        }
    }
});
if ($api !== null) $api->zend_async_socket_listen_fd_fn = $adopt;
try {
    $server->start();
    Async\await($control);
} finally {
    if ($api !== null) $api->zend_async_socket_listen_fd_fn = $original[0];
}
