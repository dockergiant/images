# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository Overview

This is the **RollDev / dockergiant Docker images** repository — a CI/CD-driven build system producing the multi-architecture (`amd64` + `arm64`) Docker images that the [`roll-docker-stack`](../roll-docker-stack) CLI consumes at runtime. All images publish to GitHub Container Registry under **`ghcr.io/dockergiant/`**.

There is almost no application code here: each top-level directory is a service with one or more `Dockerfile`s, and `.github/workflows/` contains the build pipelines. The interesting logic lives in the Dockerfiles, the PHP-FPM entrypoint, and the matrix-generation script.

### Services (top-level directories)

| Service | Dockerfile(s) | Base image | Notes |
|---------|---------------|-----------|-------|
| **php-fpm** | `Dockerfile` (+ 5 variant subdirs) | `${REGISTRY_BASE}/php:${PHP_VERSION}-fpm-${OS_RELEASE}` | Most complex image; full variant tree below |
| **nginx** | `Dockerfile` | `nginx:${NGINX_VERSION}` | Ships 10 platform configs in `etc/nginx/available.d/` |
| **varnish** | `Dockerfile`, `Dockerfile.lts`, `Dockerfile.legacy` | Alpine / Debian / CentOS | Magento-optimized `default.vcl` |
| **mysql** | `Dockerfile` | `mysql:${MYSQL_VERSION}` | Adds `skip-bin-log.cnf` |
| **mariadb** | `Dockerfile` | `mariadb:${MARIADB_VERSION}` | Symlinks `mariadb*`→`mysql*` for 11.4+ compat |
| **redis** | `Dockerfile` | `redis:${REDIS_VERSION}` | passthrough |
| **valkey** | `Dockerfile` | `valkey/valkey:${VALKEY_VERSION}` | passthrough; Redis-compatible cache for Magento 2.4.8+ |
| **mongo** | `Dockerfile` | `mongo:${MONGO_VERSION}` | passthrough |
| **dragonfly** | `Dockerfile` | `dragonflydb/dragonfly:${DRAGONFLY_VERSION}` | Redis-compatible cache |
| **elasticsearch** | `Dockerfile` | `elasticsearch:${ES_VERSION}` | + `analysis-phonetic`, `analysis-icu` plugins |
| **opensearch** | `Dockerfile` | `opensearchproject/opensearch:${OPENSEARCH_VERSION}` | + `analysis-phonetic`, `analysis-icu` plugins |
| **rabbitmq** | `Dockerfile` | `rabbitmq:${RABBITMQ_VERSION}-management` | management UI |
| **magepack** | `Dockerfile` | `zenika/alpine-chrome:with-puppeteer` | `magepack@^${MAGEPACK_VERSION}` (2.3) |
| **mailhog** | `Dockerfile` | multi-stage `golang` → `alpine` | builds MailHog; SMTP 1025 / HTTP 8025 |
| **dnsmasq** | `Dockerfile` | multi-stage `golang` → `debian` | dnsmasq + `webproc` v0.4.0 config UI |
| **startpage** | `Dockerfile` | `nginx:alpine` | dashboard with `roll-status.sh` CGI |

---

## Build Commands

### Trigger CI/CD builds

```bash
# Touch the per-service .trigger file to force a workflow run (no code change needed)
echo "$(uuidgen)" > php-fpm/.trigger && git add php-fpm/.trigger && git commit -m "Trigger build" && git push

# Manual dispatch via GitHub CLI
gh workflow run docker-image-php-fpm.yml -f build_type=latest-only
gh workflow run docker-image-php-fpm.yml -f build_type=full
gh workflow run docker-image-php-fpm.yml -f build_type=custom -f php_versions="8.2,8.3" -f node_versions="18,20"
gh workflow run docker-image-php-fpm.yml -f force_rebuild_all=true
gh workflow run mirror-base-images-php-fpm.yml -f force_mirror=true
```

### Local matrix testing (`test-matrix.sh`)

Simulates the GitHub Actions matrix logic locally (uses `crane` to check which images already exist in GHCR, so it builds only what's missing):

```bash
BUILD_TYPE=latest-only ./test-matrix.sh                                   # latest PHP + top 2 Node (default)
BUILD_TYPE=full ./test-matrix.sh                                          # all PHP + latest 5 Node
BUILD_TYPE=custom PHP_VERSIONS="8.2,8.3" NODE_VERSIONS="18,20" ./test-matrix.sh
FORCE_BUILD_BASE=true ./test-matrix.sh                                    # rebuild base php-fpm even if it exists
FORCE_REBUILD_ALL=true ./test-matrix.sh                                   # rebuild all variants except base
```

`BUILD_TYPE` ∈ `latest-only | full | custom`. It discovers PHP (7.4+) and Node (10+) versions from the mirrored base images, generates the build matrix for the base image + node/xdebug/magento1/magento2/wordpress variants, and prints "will build" vs "skipping" per image.

### Build images locally

```bash
# PHP-FPM base + variants (variants build FROM the published base image)
docker build -t php-fpm:8.4 --build-arg PHP_VERSION=8.4 --build-arg OS_RELEASE=bookworm php-fpm/
docker build -t php-fpm-node:8.4-20  --build-arg PHP_VERSION=8.4 --build-arg NODE_VERSION=20 php-fpm/node/
docker build -t php-fpm-xdebug:8.4   --build-arg PHP_VERSION=8.4 php-fpm/xdebug3/
docker build -t php-fpm-magento2:8.4 --build-arg PHP_VERSION=8.4 php-fpm/magento2/
docker build -t php-fpm-wordpress:8.4 --build-arg PHP_VERSION=8.4 php-fpm/wordpress/

# Databases & cache
docker build -t redis:7.4   --build-arg REDIS_VERSION=7.4   redis/
docker build -t valkey:9.0  --build-arg VALKEY_VERSION=9.0  valkey/
docker build -t mysql:8.0   --build-arg MYSQL_VERSION=8.0   mysql/
docker build -t mariadb:11.4 --build-arg MARIADB_VERSION=11.4 mariadb/

# Web & proxy
docker build -t nginx:latest nginx/
docker build -t varnish:7.6  --build-arg VARNISH_VERSION=7.6 varnish/
docker build -t varnish:6.0-lts -f varnish/Dockerfile.lts varnish/

# Search & queue
docker build -t elasticsearch:8.17.0 --build-arg ES_VERSION=8.17.0 elasticsearch/
docker build -t rabbitmq:3.13 --build-arg RABBITMQ_VERSION=3.13 rabbitmq/
```

---

## PHP-FPM (the complex one)

### Variant hierarchy

```
php-fpm (base)                         # FROM ${REGISTRY_BASE}/php:${PHP_VERSION}-fpm-${OS_RELEASE}
├── node/        (+ Node.js, npm, yarn, gulp, grunt, PhantomJS)   # FROM base + node + phantomjs stages
├── xdebug3/     (+ Xdebug 3, IDE integration)
├── magento1/    (+ n98-magerun)        ├── blackfire/   └── xdebug3/
├── magento2/    (+ n98-magerun2, cache-clean)  ├── blackfire/   └── xdebug3/
└── wordpress/   (+ WP-CLI)             ├── blackfire/   └── xdebug3/
```

Variants build `FROM ${ENV_SOURCE_IMAGE}:${PHP_VERSION}` (the published base), so the base must exist before variants build. The `node` and base Dockerfiles are multi-stage and pull `composer`, `node`, `phantomjs`, and `mhsendmail` from helper stages / the mirror registry.

### Base build args (`php-fpm/Dockerfile`)

| ARG | Default | Purpose |
|-----|---------|---------|
| `PHP_VERSION` | `8.4` | PHP major.minor |
| `OS_RELEASE` | `bookworm` | Debian release (`buster`/`bullseye`/`bookworm`, chosen by PHP version) |
| `ENV_SOURCE_IMAGE` | `ghcr.io/dockergiant/php-fpm` | base image variants build FROM |
| `REGISTRY_BASE` | `ghcr.io/dockergiant/base-images` | mirror registry for php/node/composer/phantomjs |

### PHP extensions

Always installed: `apcu`, `amqp`, `bcmath`, `calendar`, `exif`, `gd`, `intl`, `imap`, `mysqli`, `pcntl`, `pdo_mysql`, `redis`, `soap`, `sockets`, `sodium`, `xsl`, `zip`.

Version/arch-conditional (pattern: `printf "X.Y\n${PHP_VERSION}" | sort -g | head -n1`):
- `imagick` — PHP < 8.3
- `mcrypt` — PHP < 8.2
- `ftp` — PHP ≥ 8.2
- `newrelic` — x86_64 PHP ≥ 7.0, arm64 PHP ≥ 8.0

Tools baked in: composer (v1/v2/v2.2-LTS, switchable), `mhsendmail`, git, vim, nano, htop, jq, rsync, imagemagick/graphicsmagick, chromium, socat, cron, zsh/fish, mariadb-client, python3.

### Entrypoint (`php-fpm/context/docker-entrypoint`)

Runs in order on container start:
1. **User/group remap** — on Linux, remap `www-data` to host `USER_ID:GROUP_ID` (defaults `501:20` for macOS parity).
2. **MailHog** — `envsubst` `${MAILHOG_HOST}`/`${MAILHOG_PORT}` into `05-additions.ini` (sets `sendmail_path` → `mhsendmail`).
3. **New Relic** — if `ROLL_NEWRELIC=1` + `NEWRELIC_LICENSE_KEY`; app named `RollDev-LocalEnv-${ROLL_ENV_NAME}`; arch-aware.
4. **SSL CA** — install Roll root CA from `/etc/ssl/roll-rootca-cert/ca.cert.pem` into system trust.
5. **SSH agent forwarding** — `socat` bridge to host `/run/host-services/ssh-auth.sock`.
6. **Cron** — start `service cron` in background.
7. **Composer version** — symlink `/usr/bin/composer` per `COMPOSER_VERSION`.
8. **Volume perms** — chown dirs listed in `CHOWN_DIR_LIST`.
9. **Runtime extensions** — `install-php-extensions $ADD_PHP_EXT`.

### Config files

`context/etc/php.d/`: `01-php.ini` (timezone UTC, `max_execution_time=3600`, `upload_max_filesize`/`post_max_size=4096M`, `max_input_vars=10000`), `05-additions.ini.template` (MailHog sendmail), `10-opcache.ini` (`memory_consumption=512M`, `max_accelerated_files=65407`), `newrelic.ini.template`.

`context/etc/profile.d/`: `bind.sh`, `colorgrep.sh`, `colorls.sh`, `path.sh`, `ps1.sh`, `vim.sh` (shell UX).

### Xdebug 3 (`xdebug3/`)

```ini
xdebug.mode=debug,trace,profile
xdebug.discover_client_host=on
xdebug.client_discovery_header=HTTP_X_DEBUG_HOST
xdebug.idekey=PHPSTORM
xdebug.start_with_request=trigger   # FPM (yes for CLI)
xdebug.file_link_format=phpstorm://open?file=%f&line=%l
```

---

## Nginx

10 platform templates in `etc/nginx/available.d/` — selected at runtime via `NGINX_TEMPLATE`:
`application.conf` (default), `magento2.conf`, `magento2-dev.conf`, `magento2-autologin.conf`, `magento2-dev-autologin.conf`, `magento1.conf`, `magento1-dev.conf`, `laravel.conf`, `typo3.conf`, `vuejs.conf`.

Key env vars: `NGINX_TEMPLATE`, `NGINX_ROOT=/var/www/html`, `NGINX_PUBLIC=/pub`, `NGINX_UPSTREAM_HOST=php-fpm`, `NGINX_UPSTREAM_PORT=9000`, `NGINX_UPSTREAM_DEBUG_HOST=php-debug`, `NGINX_UPSTREAM_BLACKFIRE_HOST=php-blackfire`.

**Xdebug routing:** requests are routed to the `php-debug` upstream automatically when an `XDEBUG_SESSION`, `XDEBUG_PROFILE`, or `XDEBUG_TRACE` cookie is present.

---

## Varnish

| Dockerfile | Base | Versions | Use |
|------------|------|----------|-----|
| `Dockerfile` | Alpine | 7.1–7.7 | current (recommended) |
| `Dockerfile.lts` | Debian | 6.0 LTS | long-term support |
| `Dockerfile.legacy` | CentOS Stream 8 | 6.5 | legacy |

`default.vcl` is Magento-optimized: `X-Magento-Tags-Pattern` ban-purging, `X-Magento-Vary` cache keys, GraphQL `Cache-Id` hashing + bearer awareness, ESI support, marketing-param stripping (`utm_*`/`gclid`/`fbclid`), 1h grace, PURGE ACL.

Env: `VCL_CONFIG=/etc/varnish/default.vcl`, `CACHE_SIZE=256m`, `BACKEND_HOST=nginx`, `BACKEND_PORT=80`, `ACL_PURGE_HOST=0.0.0.0/0` (restrict in prod), `VARNISHD_PARAMS`.

---

## CI/CD Workflows (`.github/workflows/`)

**Dynamic discovery (crane-based):**
- `docker-image-php-fpm.yml` — inputs `build_type` (latest-only/full/custom), `php_versions`, `node_versions`, `force_build_base`, `force_rebuild_all`. Builds base + all variants.
- `docker-image-nginx.yml` — input `force_rebuild`.

**Static matrix (one version list per service):**
- `docker-image-varnish.yml` (7.1/7.4/7.6/6.0/6.5)
- `docker-image-mysql.yml` (5.5/5.7/8.0/9.0)
- `docker-image-mariadb.yml` (10.5/10.6/10.11/11.0/11.4)
- `docker-image-redis.yml` (6.0/6.2/7.0/7.2/7.4/8.0)
- `docker-image-valkey.yml` (discovers `valkey/valkey` major.minor tags from 8.0, publishes `ghcr.io/dockergiant/valkey:<major.minor>`)
- `docker-image-mongo.yml` (5.0/6.0/7.0)
- `docker-image-elasticsearch.yml` (7.17/8.0/8.11/8.17.0)
- `docker-image-opensearch.yml` (1.3/2.0/2.5/2.11)
- `docker-image-rabbitmq.yml` (3.11/3.12/3.13)
- `docker-image-dragonflydb.yml` (latest/1.0.0)

**Single build (no matrix):** `docker-image-magepack.yml`, `docker-image-mailhog.yml`, `docker-image-dnsmasq.yml`, `docker-image-startpage.yml`.

**Support:**
- `mirror-base-images-php-fpm.yml` — mirrors PHP/Node/Composer/PhantomJS base images Docker Hub → `ghcr.io/dockergiant/base-images/`. Weekly (Sun 02:00 UTC) or manual. See `PHP-FPM-MIRRORING.md`.
- `shellcheck.yml` — lint all `.sh` files.

**Triggers (common):** `workflow_dispatch`, push/PR with path filters (service dir, workflow file, or that dir's `.trigger`), and `workflow_run` (php-fpm runs after the mirror workflow completes).

### `.trigger` files

Each service dir can hold a `.trigger` file (e.g. `php-fpm/.trigger`). Overwriting it with a fresh UUID and pushing forces that service's workflow to run without any real code change.

### Base-image mirroring strategy (`PHP-FPM-MIRRORING.md`)

To avoid Docker Hub rate limits in CI, base images are mirrored to GHCR and the Dockerfiles pull from `${REGISTRY_BASE}` (default `ghcr.io/dockergiant/base-images`): `php`, `node`, `composer`, `phantomjs`. The mirror workflow auto-discovers versions via `crane`, picks the OS release per PHP version (7.4→buster, 8.0/8.1→bullseye, 8.2+→bookworm), and falls back to Docker Hub if a mirror is unavailable.

---

## Conventions & Patterns

- **Multi-arch:** images build for `amd64` + `arm64`; keep extension/tool installs arch-aware where binaries differ (see New Relic logic).
- **Version-conditional installs:** use the `printf "X.Y\n${VERSION}" | sort -g | head -n1` comparison idiom already established in `php-fpm/Dockerfile` — don't introduce a different version-compare style.
- **Registry layout:** primary `ghcr.io/dockergiant/{image}:{version}`; base mirrors under `ghcr.io/dockergiant/base-images/{php,node,composer,phantomjs}`; Docker Hub fallback.
- **Build-before-skip:** CI/`test-matrix.sh` check image existence via `crane manifest` before building — preserve this so reruns are cheap.
- **ShellCheck:** all `.sh` scripts are linted in CI; aim for POSIX compatibility.
- **Adding a service:** create `<service>/Dockerfile`, an optional `<service>/.trigger`, and a `docker-image-<service>.yml` workflow (static matrix unless versions must be auto-discovered).

## Related Repos

- **`../roll-docker-stack`** — the `roll` CLI that pulls and runs these images per project.
