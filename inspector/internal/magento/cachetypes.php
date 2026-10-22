<?php
ini_set('display_errors', 'stderr');
error_reporting(E_ALL & ~E_DEPRECATED & ~E_USER_DEPRECATED);

require 'app/bootstrap.php';

$bootstrap = \Magento\Framework\App\Bootstrap::create(BP, $_SERVER);
$types = $bootstrap->getObjectManager()->get(\Magento\Framework\App\Cache\TypeListInterface::class)->getTypes();

$out = [];
foreach ($types as $id => $type) {
    $out[$id] = (string) $type->getData('tags');
}

echo 'ROLL_INSPECTOR_JSON ', json_encode((object) $out, JSON_UNESCAPED_SLASHES), "\n";
