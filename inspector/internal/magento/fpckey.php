<?php
ini_set('display_errors', 'stderr');
error_reporting(E_ALL & ~E_DEPRECATED & ~E_USER_DEPRECATED);

$url = parse_url($argv[1] ?? '');
if (!is_array($url) || !isset($url['scheme'], $url['host']) || !in_array($url['scheme'], ['http', 'https'], true)) {
    fwrite(STDERR, "invalid url\n");
    exit(2);
}

$secure = $url['scheme'] === 'https';
$port = $url['port'] ?? ($secure ? 443 : 80);
$path = ($url['path'] ?? '') === '' ? '/' : $url['path'];
$query = $url['query'] ?? '';
$host = $url['host'] . (isset($url['port']) ? ':' . $url['port'] : '');

$_SERVER['REQUEST_METHOD'] = 'GET';
$_SERVER['HTTP_HOST'] = $host;
$_SERVER['SERVER_NAME'] = $url['host'];
$_SERVER['SERVER_PORT'] = (string) $port;
$_SERVER['REQUEST_URI'] = $path . ($query === '' ? '' : '?' . $query);
$_SERVER['QUERY_STRING'] = $query;
if ($secure) {
    $_SERVER['HTTPS'] = 'on';
} else {
    unset($_SERVER['HTTPS']);
}
unset($_SERVER['argv'], $_SERVER['argc']);
$_GET = [];
parse_str($query, $_GET);

require 'app/bootstrap.php';

$bootstrap = \Magento\Framework\App\Bootstrap::create(BP, $_SERVER);
$om = $bootstrap->getObjectManager();
$om->get(\Magento\Framework\App\State::class)->setAreaCode('frontend');
$om->configure($om->get(\Magento\Framework\ObjectManager\ConfigLoaderInterface::class)->load('frontend'));

$key = $om->get(\Magento\Framework\App\PageCache\Identifier::class)->getValue();

echo 'ROLL_INSPECTOR_JSON ', json_encode(['key' => $key], JSON_UNESCAPED_SLASHES), "\n";
