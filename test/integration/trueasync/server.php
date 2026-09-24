<?php
// Control experiment: native HTTP/1.1 and h2c with its own listener.
// This is deliberately NOT claimed as ooth socket activation.
use TrueAsync\HttpServer;
use TrueAsync\HttpServerConfig;

$server = new HttpServer(
    (new HttpServerConfig())->addListener('127.0.0.1', (int) $argv[1])->setWorkers(1)
);
$server->addHttpHandler(function ($request, $response) {
    Async\delay(5);
    $response->setStatusCode(200)->setHeader('Content-Type', 'text/plain')->setBody("trueasync native\n");
});
$server->start();
