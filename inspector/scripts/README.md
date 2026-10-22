# Inspector scripts

## php-matrix

`php-matrix` sends three requests through a php-fpm image and checks what the tracer and the Excimer prepend deliver to the Inspector intake. A capture server answers as `inspector:8126` on a throwaway Docker network, and the `phpmatrix` Go test reads what it captured.

    scripts/php-matrix 8.5 8.3
    scripts/php-matrix --build 8.0 8.1 8.2 8.4

Each version runs four modes: full, lean, off and debug. Set `PHPMATRIX_RUNS` to a subset, for example `PHPMATRIX_RUNS="full off"`. Set `PHPMATRIX_OUT` to keep the captures in a directory you choose.

The script uses the local `ghcr.io/dockergiant/php-fpm:<version>` and `php-fpm-debug:<version>` images. `--build` builds a missing tag locally the way CI does and pushes nothing. A fresh build needs about 2.5 GB of Docker disk per version, so build one version at a time. The script removes its containers and networks when it exits.

A passing run ends with `php-matrix: all checks passed`. Results per PHP version are in `roll-docker-stack/docs/inspector/how-it-works.md`.
