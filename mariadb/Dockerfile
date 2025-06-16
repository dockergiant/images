ARG MARIADB_VERSION
FROM mariadb:${MARIADB_VERSION}

# Fix for MariaDB 11.4+ where mysqld was renamed to mariadbd
# Create symbolic link for backwards compatibility with entrypoint scripts
RUN if [ -f /usr/sbin/mariadbd ] && [ ! -f /usr/sbin/mysqld ]; then \
        ln -s /usr/sbin/mariadbd /usr/sbin/mysqld; \
    fi
