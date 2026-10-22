<?php

if (PHP_SAPI !== 'fpm-fcgi'
    || !extension_loaded('ddtrace')
    || !extension_loaded('excimer')
    || !function_exists('DDTrace\root_span')
    || \DDTrace\root_span() === null
) {
    return;
}

(static function (): void {
    $send = static function (\ExcimerProfiler $profiler): void {
        $profiler->stop();
        $span = \DDTrace\root_span();
        if ($span === null) {
            return;
        }
        $id = strtolower((string) $span->traceId);
        if (!preg_match('/^[0-9a-f]{32}$/', $id)) {
            return;
        }
        $json = json_encode($profiler->getLog()->getSpeedscopeData());
        if ($json === false || strlen($json) > 5 * 1024 * 1024) {
            return;
        }
        $ch = curl_init('http://inspector:8126/profiles/' . $id);
        if ($ch === false) {
            return;
        }
        curl_setopt_array($ch, [
            CURLOPT_POST => true,
            CURLOPT_POSTFIELDS => $json,
            CURLOPT_HTTPHEADER => ['Content-Type: application/json'],
            CURLOPT_RETURNTRANSFER => true,
            CURLOPT_CONNECTTIMEOUT_MS => 100,
            CURLOPT_TIMEOUT_MS => 500,
            CURLOPT_NOSIGNAL => true,
        ]);
        @curl_exec($ch);
    };
    $profiler = new \ExcimerProfiler();
    $profiler->setPeriod(0.005);
    $profiler->setEventType(EXCIMER_REAL);
    $profiler->start();

    // Registered from inside a shutdown function so it runs after Magento's own handlers and the root span close
    register_shutdown_function(static function () use ($profiler, $send): void {
        register_shutdown_function(static function () use ($profiler, $send): void {
            // Magento's developer-mode handler turns any notice here into an error page appended to the response
            set_error_handler(static fn (): bool => true);
            try {
                $send($profiler);
            } catch (\Throwable $e) {
            } finally {
                restore_error_handler();
            }
        });
    });
})();
