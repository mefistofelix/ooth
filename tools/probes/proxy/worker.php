<?php
// Stock php-cgi: this response uses FastCGI, never ooth's stdout protocol.
header('Content-Type: text/plain');
$delay = min(2000, max(0, (int) ($_GET['delay'] ?? 0)));
usleep($delay * 1000);
echo 'worker=' . getmypid() . " protocol=fastcgi\n";
