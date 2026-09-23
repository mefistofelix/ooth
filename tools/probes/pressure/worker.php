<?php
// Stock FastCGI: script entry is observable, the native accept timestamp is not.
$started = sprintf('%.0f', microtime(true) * 1000000000);
usleep((int) $_GET['delay'] * 1000);
header('Content-Type: text/plain');
echo (int) $_GET['id'] . ' ' . getmypid() . ' 0 ' . $started . "\n";
