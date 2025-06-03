FROM nginx

ADD docker-entrypoint /docker-entrypoint
ADD index.html /usr/share/nginx/html/index.html
ADD img/dnsmasq.svg /usr/share/nginx/html/img/dnsmasq.svg
ADD img/portainer.png /usr/share/nginx/html/img/portainer.png
ADD img/mailhog.png /usr/share/nginx/html/img/mailhog.png
ADD img/traefik.png /usr/share/nginx/html/img/traefik.png

ENTRYPOINT [ "/docker-entrypoint" ]
