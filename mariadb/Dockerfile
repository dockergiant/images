ARG MARIADB_VERSION
FROM mariadb:${MARIADB_VERSION}

# Fix for MariaDB 11.4+ where binaries were renamed
# Create symbolic links for backwards compatibility with entrypoint scripts and client tools
RUN if [ -f /usr/sbin/mariadbd ] && [ ! -f /usr/sbin/mysqld ]; then \
        ln -s /usr/sbin/mariadbd /usr/sbin/mysqld; \
    fi && \
    if [ -f /usr/bin/mariadb ] && [ ! -f /usr/bin/mysql ]; then \
        ln -s /usr/bin/mariadb /usr/bin/mysql; \
    fi && \
    if [ -f /usr/bin/mariadb-admin ] && [ ! -f /usr/bin/mysqladmin ]; then \
        ln -s /usr/bin/mariadb-admin /usr/bin/mysqladmin; \
    fi && \
    if [ -f /usr/bin/mariadb-dump ] && [ ! -f /usr/bin/mysqldump ]; then \
        ln -s /usr/bin/mariadb-dump /usr/bin/mysqldump; \
    fi && \
    if [ -f /usr/bin/mariadb-check ] && [ ! -f /usr/bin/mysqlcheck ]; then \
        ln -s /usr/bin/mariadb-check /usr/bin/mysqlcheck; \
    fi && \
    if [ -f /usr/bin/mariadb-show ] && [ ! -f /usr/bin/mysqlshow ]; then \
        ln -s /usr/bin/mariadb-show /usr/bin/mysqlshow; \
    fi && \
    if [ -f /usr/bin/mariadb-import ] && [ ! -f /usr/bin/mysqlimport ]; then \
        ln -s /usr/bin/mariadb-import /usr/bin/mysqlimport; \
    fi && \
    if [ -f /usr/bin/mariadb-binlog ] && [ ! -f /usr/bin/mysqlbinlog ]; then \
        ln -s /usr/bin/mariadb-binlog /usr/bin/mysqlbinlog; \
    fi && \
    if [ -f /usr/bin/mariadb-slap ] && [ ! -f /usr/bin/mysqlslap ]; then \
        ln -s /usr/bin/mariadb-slap /usr/bin/mysqlslap; \
    fi && \
    if [ -f /usr/bin/mariadb-hotcopy ] && [ ! -f /usr/bin/mysqlhotcopy ]; then \
        ln -s /usr/bin/mariadb-hotcopy /usr/bin/mysqlhotcopy; \
    fi
