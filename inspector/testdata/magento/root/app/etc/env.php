<?php
return [
    'backend' => ['frontName' => 'admin_planted'],
    'crypt' => ['key' => 'PLANTED-CRYPT-KEY-0001'],
    'install' => ['date' => 'Mon, 20 Oct 2026 10:00:00 +0000'],
    'MAGE_MODE' => 'developer',
    'db' => [
        'table_prefix' => 'm2_',
        'connection' => [
            'default' => [
                'host' => 'db',
                'dbname' => 'magento',
                'username' => 'PLANTED-DB-USER',
                'password' => 'PLANTED-DB-PASSWORD',
                'active' => '1',
            ],
        ],
    ],
    'cache' => [
        'graphql' => ['id_salt' => 'PLANTED-ID-SALT'],
        'frontend' => [
            'default' => [
                'id_prefix' => 'abc_',
                'backend' => 'Magento\\Framework\\Cache\\Backend\\Redis',
                'backend_options' => [
                    'server' => 'redis',
                    'port' => '6379',
                    'database' => '0',
                    'password' => 'PLANTED-CACHE-PASSWORD',
                    'compress_data' => '1',
                    'compression_lib' => 'gzip',
                    'serializer' => 'igbinary',
                ],
            ],
            'page_cache' => [
                'id_prefix' => 'abc_',
                'backend' => 'Magento\\Framework\\Cache\\Backend\\Redis',
                'backend_options' => [
                    'server' => 'redis',
                    'port' => '6379',
                    'database' => '1',
                    'password' => 'PLANTED-FPC-PASSWORD',
                    'compress_data' => '0',
                ],
            ],
        ],
    ],
    'session' => [
        'save' => 'redis',
        'redis' => [
            'host' => 'redis',
            'port' => '6379',
            'password' => 'PLANTED-SESSION-PASSWORD',
            'database' => '2',
            'sentinel_password' => 'PLANTED-SENTINEL-PASSWORD',
        ],
    ],
    'queue' => [
        'amqp' => [
            'host' => 'rabbitmq',
            'port' => '5672',
            'user' => 'planted-amqp-user',
            'password' => 'planted-amqp-password',
            'virtualhost' => '/',
            'ssl' => '',
        ],
        'consumers_wait_for_messages' => 1,
    ],
    'lock' => [
        'provider' => 'db',
        'config' => ['prefix' => 'lk_', 'token' => 'PLANTED-LOCK-TOKEN'],
    ],
    'http_cache_hosts' => [
        ['host' => 'varnish', 'port' => '80'],
    ],
    'cache_types' => [
        'config' => 1,
        'layout' => 1,
        'full_page' => 0,
        'block_html' => 1,
    ],
    'acme_support' => ['relay' => ['url' => 'https://relay.example', 'token' => 'PLANTED-RELAY-TOKEN']],
    'system' => [
        'default' => [
            'web' => [
                'unsecure' => ['base_url' => 'https://app.example.test/'],
                'secure' => [
                    'base_url' => 'https://app.example.test/',
                    'offloader_header' => 'X-Forwarded-Proto',
                ],
                'cookie' => ['cookie_domain' => 'example.test'],
            ],
            'system' => [
                'full_page_cache' => [
                    'caching_application' => '2',
                    'ttl' => '86400',
                ],
                'smtp' => ['password' => 'PLANTED-SMTP-PASSWORD'],
            ],
            'catalog' => [
                'search' => [
                    'engine' => 'opensearch',
                    'opensearch_server_hostname' => 'opensearch',
                    'opensearch_password' => 'PLANTED-SEARCH-PASSWORD',
                ],
            ],
            'payment' => [
                'stripe_payments' => [
                    'stripe_live_sk' => 'sk_live_PLANTEDSTRIPESECRET',
                    'api_key' => 'PLANTED-PAYMENT-API-KEY',
                ],
                'paypal_express' => ['api_signature' => 'PLANTED-PAYPAL-SIGNATURE'],
            ],
            'carriers' => [
                'ups' => [
                    'access_license_number' => 'PLANTED-UPS-LICENSE',
                    'password' => 'PLANTED-UPS-PASSWORD',
                    'username' => 'PLANTED-UPS-USER',
                ],
                'fedex' => ['key' => 'PLANTED-FEDEX-KEY', 'meter_number' => 'PLANTED-FEDEX-METER'],
            ],
        ],
        'websites' => [
            'nl' => [
                'web' => [
                    'unsecure' => ['base_url' => 'https://nl.example.test/'],
                    'secure' => ['base_url' => 'https://nl.example.test/'],
                ],
                'payment' => ['mollie' => ['apikey_live' => 'live_PLANTEDMOLLIEKEY']],
            ],
        ],
        'stores' => [
            'be_fr' => [
                'web' => ['secure' => ['base_url' => 'https://fr.example.test/']],
            ],
        ],
    ],
];
