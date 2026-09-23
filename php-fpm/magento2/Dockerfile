ARG ENV_SOURCE_IMAGE
ARG PHP_VERSION
FROM ${ENV_SOURCE_IMAGE}:${PHP_VERSION}
USER root

# Magerun

# PHP_VERSION here is the base image env (e.g. 8.4.12), not the build arg, which can carry a -nodeXX
# suffix. Each pin is the newest n98-magerun2 release that runs on that PHP version.
RUN case "${PHP_VERSION%.*}" in \
        7.0|7.1|7.2) MAGERUN2_VERSION=4.7.0 ;; \
        7.3) MAGERUN2_VERSION=6.1.1 ;; \
        7.4) MAGERUN2_VERSION=7.5.0 ;; \
        8.0) MAGERUN2_VERSION=8.1.1 ;; \
        8.1) MAGERUN2_VERSION=9.5.1 ;; \
        *) MAGERUN2_VERSION=10.0.2 ;; \
    esac \
    && curl -fsSL "https://files.magerun.net/n98-magerun2-${MAGERUN2_VERSION}.phar" -o /usr/local/bin/n98-magerun2

RUN chmod +rx /usr/local/bin/n98-magerun2 \
    && ln -s /usr/local/bin/n98-magerun2 /usr/local/bin/n98-magerun \
    && ln -s /usr/local/bin/n98-magerun2 /usr/local/bin/magerun2 \
    && ln -s /usr/local/bin/n98-magerun2 /usr/local/bin/magerun \
    && ln -s /usr/local/bin/n98-magerun2 /usr/local/bin/mr \
    && ln -s /usr/local/bin/n98-magerun2 /usr/local/bin/n98-magerun2.phar

# Magerun Auto-complete
ADD https://raw.githubusercontent.com/netz98/n98-magerun2/master/res/autocompletion/bash/n98-magerun2.phar.bash /etc/bash_completion.d/n98-magerun2.phar
ADD https://raw.githubusercontent.com/netz98/n98-magerun2/master/res/autocompletion/fish/n98-magerun2.phar.fish /etc/fish/completions/n98-magerun2.phar.fish
ADD https://raw.githubusercontent.com/netz98/n98-magerun2/master/res/autocompletion/zsh/n98-magerun2.plugin.zsh /usr/share/zsh/site-functions/_n98-magerun2

RUN sed -i 's/\(#compdef n98-magerun2.phar\)/\1 n98-magerun2=n98-magerun2.phar n98-magerun=n98-magerun2.phar magerun2=n98-magerun2.phar magerun=n98-magerun2.phar/' /usr/share/zsh/site-functions/_n98-magerun2 \
    && sed -i 's/\(compdef _n98_magerun2 n98-magerun2.phar\)/\1 n98-magerun2=n98-magerun2.phar n98-magerun=n98-magerun2.phar magerun2=n98-magerun2.phar magerun=n98-magerun2.phar/' /usr/share/zsh/site-functions/_n98-magerun2 \
    && sed -i 's/\(complete -f -c n98-magerun2.phar\)/\1 -c n98-magerun2 -c n98-magerun -c magerun2 -c magerun/' /etc/fish/completions/n98-magerun2.phar.fish \
    && ln -s /etc/fish/completions/n98-magerun2.phar.fish /etc/fish/completions/n98-magerun2.fish \
    && ln -s /etc/fish/completions/n98-magerun2.phar.fish /etc/fish/completions/n98-magerun.fish \
    && ln -s /etc/fish/completions/n98-magerun2.phar.fish /etc/fish/completions/magerun2.fish \
    && ln -s /etc/fish/completions/n98-magerun2.phar.fish /etc/fish/completions/magerun.fish \
    && chmod +r /etc/fish/completions/n98-magerun2.phar.fish \
    && chmod +r /etc/bash_completion.d/n98-magerun2.phar \
    && chmod +r /usr/share/zsh/site-functions/_n98-magerun2

# Mage2 TV Cache
RUN mkdir /home/www-data/mage-cache-clean \
    && cd /home/www-data/mage-cache-clean \
    && composer2 require mage2tv/magento-cache-clean \
    && ln -s /home/www-data/mage-cache-clean/vendor/bin/cache-clean.js /usr/local/bin/cache-clean.js \
    && ln -s /home/www-data/mage-cache-clean/vendor/bin/cache-clean.js /usr/local/bin/cache-clean \
    && chmod +rx -R /home/www-data/mage-cache-clean
