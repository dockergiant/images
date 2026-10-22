<?php
ini_set('display_errors', 'stderr');
error_reporting(E_ALL & ~E_DEPRECATED & ~E_USER_DEPRECATED);

require 'app/bootstrap.php';

$bootstrap = \Magento\Framework\App\Bootstrap::create(BP, $_SERVER);
$config = $bootstrap->getObjectManager()->get(\Magento\Framework\MessageQueue\Consumer\ConfigInterface::class);

$out = [];
foreach ($config->getConsumers() as $consumer) {
    $out[$consumer->getName()] = $consumer->getQueue();
}

echo 'ROLL_INSPECTOR_JSON ', json_encode((object) $out, JSON_UNESCAPED_SLASHES), "\n";
