ARG ENV_SOURCE_IMAGE
ARG PHP_VERSION
ARG OS_RELEASE=bookworm
ARG REGISTRY_BASE=ghcr.io/dockergiant/base-images

FROM golang:bullseye AS mhs-builder
RUN go install github.com/mailhog/mhsendmail@latest

# composer stages
FROM ${REGISTRY_BASE}/composer:1 AS composer1
FROM ${REGISTRY_BASE}/composer:2 AS composer2


FROM ${REGISTRY_BASE}/php:${PHP_VERSION}-fpm-${OS_RELEASE}
USER root

ENV MAILHOG_HOST    mailhog
ENV MAILHOG_PORT    1025
ENV USER_ID         501
ENV GROUP_ID        20
ENV OSTYPE          darwin220
ENV ADD_PHP_EXT     ""

# PHP Extension Installer
ADD https://github.com/mlocati/docker-php-extension-installer/releases/latest/download/install-php-extensions /usr/local/bin/

# Copy mhsendmail to PHP
COPY --from=mhs-builder /go/bin/mhsendmail /usr/local/bin/mhsendmail

# If PHP = 7.0 | debs are archived so use those / --allow-unauthenticated because gpg keys are expired
RUN if [ "${PHP_VERSION}" = "7.0" ]; \
    then printf "deb http://archive.debian.org/debian/ stretch main\ndeb-src http://archive.debian.org/debian/ stretch main" > /etc/apt/sources.list; \
    apt-get update -y; \
    apt-get install -y --no-install-recommends --allow-unauthenticated \
    python \
    python-pip; fi

# Install PHP Extensions required by Magento OS, Adobe Commerce, and the UCT (pcntl)
RUN chmod +x /usr/local/bin/install-php-extensions && install-php-extensions \
    apcu \
    amqp \
    bcmath \
    calendar \
    exif \
    gd \
    intl \
    imap \
    mysqli \
    pcntl \
    pdo_mysql \
    redis \
    soap \
    sockets \
    sodium \
    xsl \
    zip;

# If PHP < 8.3 install imagick
RUN if [ "$(printf "8.3\n${PHP_VERSION}" | sort -g | head -n1)" != "8.3" ]; \
    then install-php-extensions imagick; fi

# If PHP < 8.2 install mcrypt
RUN if [ "$(printf "8.2\n${PHP_VERSION}" | sort -g | head -n1)" != "8.2" ]; \
    then install-php-extensions mcrypt; fi

# If PHP >= 8.2 install ftp
RUN if [ "$(printf "8.2\n${PHP_VERSION}" | sort -g | head -n1)" = "8.2" ]; \
    then install-php-extensions ftp; fi

# Install New Relic PHP agent (available for PHP 7.0+)
RUN if [ "$(printf "7.0\n${PHP_VERSION}" | sort -g | head -n1)" = "7.0" ]; then \
    # PHP 7.0+ - install New Relic using install-php-extensions
    install-php-extensions newrelic && \
    # Disable New Relic by default - will be enabled conditionally via entrypoint
    rm -f /usr/local/etc/php/conf.d/docker-php-ext-newrelic.ini; \
fi

# Install helpful utilities
RUN set -eux; \
    export DEBIAN_FRONTEND=noninteractive; \
    apt-get update -y; \
    for i in 1 2 3 4 5; do \
      apt-get install -y --no-install-recommends --fix-missing \
        jq \
        bash \
        patch \
        pv \
        procps \
        nano \
        vim \
        mariadb-client \
        sudo \
        gettext \
        ca-certificates \
        git \
        rsync \
        bash-completion \
        nmap  \
        less \
        zsh \
        fish \
        pwgen \
        openssh-client \
        unzip \
        rsync \
        socat \
        oathtool \
        wget \
        ack  \
        automake  \
        autoconf \
        cron \
        htop \
        chromium \
        imagemagick \
        graphicsmagick \
        qrencode && break || { echo "Install failed... retrying ($i/5)" >&2; sleep 10; }; \
    done


COPY docker-entrypoint /usr/local/bin/
COPY etc/profile.d/*.sh /etc/profile.d/
COPY etc/php.d/*.ini /usr/local/etc/php/conf.d
COPY etc/php.d/05-additions.ini.template /usr/local/etc/php/conf.d
COPY etc/php.d/newrelic.ini.template /usr/local/etc/php/conf.d

RUN set -eux \
    && cp ${PHP_INI_DIR}/php.ini-development ${PHP_INI_DIR}/php.ini \
    && perl -pi -e 's/memory_limit(.*)/memory_limit=-1/g' ${PHP_INI_DIR}/php.ini

# Copy composer binaries from stages
COPY --from=composer1 /usr/bin/composer /usr/bin/composer1
COPY --from=composer2 /usr/bin/composer /usr/bin/composer2lts
COPY --from=composer2 /usr/bin/composer /usr/bin/composer2

# If PHP < 7.2
RUN if [ "$(printf "7.2\n${PHP_VERSION}" | sort -g | head -n1 | awk -F"." '{print $1"."$2}')" != "7.2" ]; \
    then mv -f /usr/bin/composer2lts /usr/bin/composer2; fi


# Install Python and pip for all PHP versions
RUN apt-get install -y --no-install-recommends libmariadb3 libmariadb-dev python3 python3-pip; \
    sed -i '/disable ghostscript format types/,+6d' /etc/ImageMagick-6/policy.xml; \
    if [ ! -f "/usr/bin/pip" ]; then ln -s /usr/bin/pip3 /usr/bin/pip; ln -s /usr/bin/python3 /usr/bin/python; fi


# Install bind9-utils based on PHP version (with proper dependency resolution)
RUN set -eux; \
    export DEBIAN_FRONTEND=noninteractive; \
    if [ "$(printf "7.3\n${PHP_VERSION}" | sort -g | head -n1 | awk -F"." '{print $1"."$2}')" != "7.3" ]; then \
        apt-get update -y; \
        apt-get install -y --no-install-recommends bind9utils; \
    else \
        apt-get update -y; \
        apt-get install -y --no-install-recommends --fix-broken bind9-utils || \
        apt-get install -y --no-install-recommends dnsutils; \
    fi

    
RUN if command -v pip >/dev/null 2>&1; then \
      # Try apt package first, fallback to pip with --break-system-packages
      apt-get update && apt-get install -y python3-segno 2>/dev/null || \
      pip install segno --break-system-packages 2>/dev/null || \
      echo "segno installation failed, continuing without it"; \
    else \
      echo "pip not found, skipping segno installation"; \
    fi

RUN mkdir -p /etc/zsh/zshrc.d  \
    && echo "autoload -U compinit; compinit" > /etc/zsh/zshrc.d/completions.zsh \
    && { [ -e /bin/fish ] || ln -s /usr/bin/fish /bin/fish; }

RUN set -eux; \
	{ \
		echo 'if [ -d /etc/profile.d ]; then'; \
        echo '  for i in /etc/profile.d/*.sh; do'; \
        echo '    if [ -r $i ]; then'; \
        echo '      . $i'; \
        echo '    fi'; \
        echo '  done'; \
        echo '  unset i'; \
        echo 'fi'; \
	} >> /etc/bash.bashrc;

# Install Oh My Zsh including the following Plugins: git, composer and n98-magerun
RUN sh -c "$(curl -fsSL https://raw.github.com/ohmyzsh/ohmyzsh/master/tools/install.sh)"
RUN git clone https://github.com/zsh-users/zsh-autosuggestions.git $ZSH_CUSTOM/plugins/zsh-autosuggestions \
    && git clone https://github.com/zsh-users/zsh-syntax-highlighting.git $ZSH_CUSTOM/plugins/zsh-syntax-highlighting \
    && sed -i 's/plugins=(git)/plugins=(git composer n98-magerun zsh-autosuggestions zsh-syntax-highlighting)/g' /root/.zshrc

# Configure www-data user as primary php-fpm user for better local dev experience
RUN mkdir -p /home/www-data \
    && chown www-data:www-data /home/www-data \
    && usermod -d /home/www-data www-data \
    && install -d -m 0755 -o www-data -g www-data /home/www-data \
    && chmod 0755 ~www-data \
    && mkdir -p /var/www/html \
    && chown www-data:www-data /var/www/html \
    && echo "www-data ALL=(ALL) NOPASSWD:ALL" >> /etc/sudoers.d/nopasswd


# clean up apt packages and temporary files
RUN set -eux; \
    apt-get clean; \
    rm -rf /var/lib/apt/lists/* /var/cache/apt/archives/*

STOPSIGNAL SIGQUIT

ENTRYPOINT ["docker-entrypoint", "php-fpm"]
WORKDIR /var/www/html
EXPOSE 9000