<?php
ini_set('display_errors', 'stderr');
error_reporting(E_ALL & ~E_DEPRECATED & ~E_USER_DEPRECATED);

const SECRET = '/(^|_)(password|pwd|passwd|secret|token|key|apikey|signature|cert)(_|$)/i';
const EXACT = ['username', 'user', 'userid', 'id_salt'];

function scrub($v)
{
    if (!is_array($v)) {
        return $v;
    }
    $out = [];
    foreach ($v as $k => $item) {
        if (is_string($k) && (preg_match(SECRET, $k) || in_array(strtolower($k), EXACT, true))) {
            continue;
        }
        $out[$k] = scrub($item);
    }
    return $out;
}

function pick(array $src, array $keys)
{
    $out = [];
    foreach ($keys as $k) {
        if (array_key_exists($k, $src)) {
            $out[$k] = $src[$k];
        }
    }
    return $out;
}

function dig($src, array $path)
{
    foreach ($path as $k) {
        if (!is_array($src) || !array_key_exists($k, $src)) {
            return null;
        }
        $src = $src[$k];
    }
    return $src;
}

function scope(array $cfg)
{
    $out = [];
    foreach ([['web', 'unsecure', 'base_url'], ['web', 'secure', 'base_url'], ['web', 'secure', 'offloader_header']] as $path) {
        $v = dig($cfg, $path);
        if ($v !== null && !is_array($v)) {
            $out[$path[0]][$path[1]][$path[2]] = $v;
        }
    }
    $fpc = dig($cfg, ['system', 'full_page_cache']);
    if (is_array($fpc)) {
        $out['system']['full_page_cache'] = scrub($fpc);
    }
    $search = dig($cfg, ['catalog', 'search']);
    if (is_array($search)) {
        $out['catalog']['search'] = scrub($search);
    }
    return $out;
}

function framework()
{
    $file = 'vendor/composer/installed.json';
    if (!is_file($file)) {
        return [null, null];
    }
    $json = json_decode((string) file_get_contents($file), true);
    $packages = is_array($json) && isset($json['packages']) ? $json['packages'] : $json;
    if (!is_array($packages)) {
        return [null, null];
    }
    $versions = [];
    foreach ($packages as $p) {
        if (isset($p['name'], $p['version'])) {
            $versions[$p['name']] = $p['version'];
        }
    }
    foreach (['mage-os', 'magento'] as $vendor) {
        if (!isset($versions["$vendor/framework"])) {
            continue;
        }
        $product = $versions["$vendor/product-community-edition"] ?? $versions["$vendor/magento2-base"] ?? $versions["$vendor/framework"];
        return ["$vendor/framework", $product];
    }
    return [null, null];
}

function main()
{
    $file = 'app/etc/env.php';
    if (!is_file($file)) {
        return ['generic' => true];
    }
    $env = include $file;
    if (!is_array($env)) {
        $env = [];
    }

    [$framework, $version] = framework();
    $out = [
        'generic' => $framework === null,
        'mtime' => filemtime($file),
        'igbinary' => extension_loaded('igbinary'),
        'framework' => $framework,
        'frameworkVersion' => $version,
        'env' => [],
    ];
    $picked = &$out['env'];

    foreach (['default', 'page_cache'] as $name) {
        $fe = dig($env, ['cache', 'frontend', $name]);
        if (!is_array($fe)) {
            continue;
        }
        $row = pick($fe, ['backend', 'id_prefix']);
        if (isset($fe['backend_options']) && is_array($fe['backend_options'])) {
            $row['backend_options'] = pick($fe['backend_options'], ['server', 'port', 'database', 'compress_data', 'compression_lib', 'serializer']);
        }
        $picked['cache']['frontend'][$name] = $row;
    }

    $session = $env['session'] ?? null;
    if (is_array($session)) {
        $picked['session'] = pick($session, ['save']);
        foreach (['redis', 'valkey'] as $block) {
            if (isset($session[$block]) && is_array($session[$block])) {
                $picked['session'][$block] = scrub($session[$block]);
            }
        }
    }

    $amqp = dig($env, ['queue', 'amqp']);
    if (is_array($amqp)) {
        $picked['queue']['amqp'] = pick($amqp, ['host', 'port', 'virtualhost', 'user', 'password']);
    }

    foreach (['http_cache_hosts', 'cache_types', 'MAGE_MODE'] as $k) {
        if (array_key_exists($k, $env)) {
            $picked[$k] = $env[$k];
        }
    }
    if (isset($env['lock'])) {
        $picked['lock'] = scrub($env['lock']);
    }

    $db = dig($env, ['db', 'connection', 'default']);
    if (is_array($db)) {
        $picked['db']['connection']['default'] = pick($db, ['host', 'dbname']);
    }
    $prefix = dig($env, ['db', 'table_prefix']);
    if ($prefix !== null) {
        $picked['db']['table_prefix'] = $prefix;
    }

    $system = $env['system'] ?? null;
    if (is_array($system)) {
        if (isset($system['default']) && is_array($system['default'])) {
            $s = scope($system['default']);
            if ($s) {
                $picked['system']['default'] = $s;
            }
        }
        foreach (['websites', 'stores'] as $type) {
            if (!isset($system[$type]) || !is_array($system[$type])) {
                continue;
            }
            foreach ($system[$type] as $code => $cfg) {
                if (!is_array($cfg)) {
                    continue;
                }
                $s = scope($cfg);
                if ($s) {
                    $picked['system'][$type][(string) $code] = $s;
                }
            }
        }
    }

    if (!$picked) {
        $out['env'] = new stdClass();
    }
    return $out;
}

echo 'ROLL_INSPECTOR_JSON ', json_encode(main(), JSON_UNESCAPED_SLASHES | JSON_PARTIAL_OUTPUT_ON_ERROR), "\n";
