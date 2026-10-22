<?php
// Regenerate entries.json: docker run --rm -i --entrypoint php ghcr.io/dockergiant/php-fpm-magento2:8.5-node24 < gen-entries.php > entries.json
// Builds the cache entries the way Mage-OS 3.x (Symfony layout) and Cm_Cache_Backend_Redis write them.

$page = json_encode([
    'content' => "<!doctype html>\n<html><head><title>Home page</title></head><body>Welcome</body></html>",
    'status_code' => 200,
    'headers' => ['Content-Type' => ['name' => 'Content-Type', 'value' => 'text/html; charset=UTF-8']],
    'context' => ['data' => ['customer_group' => '0', 'customer_logged_in' => false], 'default' => ['customer_group' => '0']],
]);
$config = serialize(['web' => ['unsecure' => ['base_url' => 'https://app.example.test/']]]);
$mtime = 1792528681;

$wrap = fn ($data, array $tags, ?int $expire) => serialize([
    'data' => $data,
    'mtime' => $mtime,
    'expire' => $expire,
    'tags' => $tags,
]);

$symfony = [
    'fast-text' => serialize('plain cached value'),
    'page-json' => $wrap($page, ['FPC', 'CMS_P_2'], null),
    'page-compressed' => $wrap('CACHE_COMPRESSION' . gzcompress($page, 6), ['FPC', 'CAT_C_41'], $mtime + 86400),
    'config-php' => $wrap($config, ['CONFIG'], $mtime + 7200),
    'encrypted' => $wrap('1:3:' . base64_encode(random_bytes(48)), ['CONFIG'], $mtime + 7200),
    'binary' => serialize("\x00\x01\x02\xff\xfe"),
];

$cm = [
    'text' => ['d' => 'plain cached value', 't' => '69d_CONFIG,69d_MAGE', 'm' => (string)$mtime, 'i' => '0'],
    'page-gz' => ['d' => "gz:\x1f\x8b" . gzcompress($page, 1), 't' => '69d_FPC,69d_CAT_C_41', 'm' => (string)$mtime, 'i' => '0'],
    'config-php' => ['d' => $config, 't' => '69d_CONFIG', 'm' => (string)$mtime, 'i' => '1'],
    'encrypted' => ['d' => '1:3:' . base64_encode(random_bytes(48)), 't' => '', 'm' => (string)$mtime, 'i' => '0'],
    'link' => ['d' => 'l:' . bin2hex(random_bytes(16)), 't' => '69d_EAV', 'm' => (string)$mtime, 'i' => '0'],
    'snappy' => ['d' => "sn:\x1f\x8b" . random_bytes(24), 't' => '69d_LAYOUT_GENERAL_CACHE_TAG', 'm' => (string)$mtime, 'i' => '0'],
];

$out = ['symfony' => [], 'cm' => []];
foreach ($symfony as $name => $raw) {
    $out['symfony'][$name] = base64_encode($raw);
}
foreach ($cm as $name => $hash) {
    $out['cm'][$name] = array_map('base64_encode', $hash);
}
echo json_encode($out, JSON_PRETTY_PRINT | JSON_UNESCAPED_SLASHES), "\n";
