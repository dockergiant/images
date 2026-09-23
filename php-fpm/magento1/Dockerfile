ARG ENV_SOURCE_IMAGE
ARG PHP_VERSION
FROM ${ENV_SOURCE_IMAGE}:${PHP_VERSION}
USER root

# n98-magerun is archived upstream and the 3.0.1 phar does not start, so stay on 2.3.0. On PHP 8.1+
# it raises deprecations that Magento turns into exceptions in developer mode, hence the wrapper.
ADD https://files.magerun.net/n98-magerun-2.3.0.phar /usr/local/lib/n98-magerun.phar
RUN chmod +r /usr/local/lib/n98-magerun.phar \
    && printf '#!/bin/sh\nexec php -d "error_reporting=E_ALL & ~E_DEPRECATED" /usr/local/lib/n98-magerun.phar "$@"\n' \
        > /usr/local/bin/n98-magerun \
    && chmod +rx /usr/local/bin/n98-magerun \
    && ln -s /usr/local/bin/n98-magerun /usr/local/bin/magerun \
    && ln -s /usr/local/bin/n98-magerun /usr/local/bin/mr

ADD https://raw.githubusercontent.com/netz98/n98-magerun/master/res/autocompletion/bash/n98-magerun.phar.bash /etc/bash_completion.d/n98-magerun.phar
RUN chmod +r /etc/bash_completion.d/n98-magerun.phar
