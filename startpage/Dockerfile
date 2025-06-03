FROM nginx:alpine

# Install bash, curl, jq, fcgiwrap
RUN apk add --no-cache bash curl jq spawn-fcgi fcgiwrap

# Add CGI script
COPY cgi-bin/roll-status.sh /usr/lib/cgi-bin/roll-status.sh
RUN chmod +x /usr/lib/cgi-bin/roll-status.sh

# Add nginx config
COPY nginx.conf /etc/nginx/nginx.conf

# Add your static dashboard files
COPY index.html /usr/share/nginx/html/index.html
COPY img/ /usr/share/nginx/html/img/

# Add custom entrypoint
COPY docker-entrypoint /docker-entrypoint
RUN chmod +x /docker-entrypoint

ENTRYPOINT ["/docker-entrypoint"]
