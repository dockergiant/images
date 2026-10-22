<?php
// Kept to PHP 7.4 syntax so every line in the matrix can run it.

function matrix_db()
{
    if (extension_loaded('pdo_sqlite')) {
        $pdo = new PDO('sqlite::memory:');
        $driver = 'sqlite';
    } else {
        $pdo = new PDO('mysql:host=mysql;dbname=matrix', 'root', 'matrix');
        $driver = 'mysql';
    }
    $pdo->setAttribute(PDO::ATTR_ERRMODE, PDO::ERRMODE_EXCEPTION);
    $pdo->exec('DROP TABLE IF EXISTS matrix_item');
    $pdo->exec('CREATE TABLE matrix_item (id INTEGER PRIMARY KEY, name VARCHAR(64))');
    $insert = $pdo->prepare('INSERT INTO matrix_item (id, name) VALUES (?, ?)');
    for ($i = 1; $i <= 10; $i++) {
        $insert->execute([$i, 'item ' . $i]);
    }
    $rows = 0;
    for ($i = 1; $i <= 10; $i++) {
        $select = $pdo->prepare('SELECT name FROM matrix_item WHERE id = ?');
        $select->execute([$i]);
        $rows += count($select->fetchAll());
    }
    return [$driver, $rows];
}

function matrix_redis()
{
    $redis = new Redis();
    $redis->connect('redis', 6379, 2.0);
    $hits = 0;
    for ($i = 0; $i < 10; $i++) {
        $redis->set('matrix:' . $i, (string) $i);
        if ($redis->get('matrix:' . $i) === (string) $i) {
            $hits++;
        }
    }
    return $hits;
}

function matrix_curl()
{
    $ch = curl_init('http://nginx/ping');
    curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);
    curl_setopt($ch, CURLOPT_TIMEOUT, 5);
    $body = curl_exec($ch);
    return is_string($body) ? trim($body) : '';
}

function matrix_burn($ms)
{
    $until = microtime(true) + $ms / 1000;
    $h = '';
    while (microtime(true) < $until) {
        for ($i = 0; $i < 2000; $i++) {
            $h = md5($h . $i);
        }
    }
    return $h;
}

[$driver, $rows] = matrix_db();
$hits = matrix_redis();
$pong = matrix_curl();
matrix_burn(150);

$traceId = '';
if (function_exists('DDTrace\root_span') && \DDTrace\root_span() !== null) {
    $traceId = strtolower((string) \DDTrace\root_span()->traceId);
}

header('Content-Type: application/json');
echo json_encode([
    'php' => PHP_VERSION,
    'sapi' => PHP_SAPI,
    'trace_id' => $traceId,
    'ddtrace_loaded' => extension_loaded('ddtrace'),
    'ddtrace_version' => (string) phpversion('ddtrace'),
    'excimer_loaded' => extension_loaded('excimer'),
    'xdebug_loaded' => extension_loaded('xdebug'),
    'xdebug_mode' => (string) ini_get('xdebug.mode'),
    'xdebug_start' => (string) ini_get('xdebug.start_with_request'),
    'magento_enabled_ini' => (string) ini_get('datadog.trace.magento_enabled'),
    'auto_prepend_file' => (string) ini_get('auto_prepend_file'),
    'db' => $driver,
    'db_rows' => $rows,
    'redis_hits' => $hits,
    'ping' => $pong,
]), "\n";
